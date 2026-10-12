package subscriptions

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/pricing"
)

// 后台手动授予：不经任何支付渠道，直接给用户开基础档位或存储。
// 眼下的用途是测试账号（商店账号都还没开），将来也可以用来补偿或赠送。
//
// 落成 payment_provider='admin' 的普通订阅行，而不是另开一张「赠送」表：
// 档位判定（wallet.PlanState）、存储配额（quota.Storage）、降级清理
// （snippet.RestrictionSweeper）都只看 subscriptions，写成同一种行，
// 这些路径一行不用改就认得它。
//
// 手动行没有到期日，只能手动撤销 —— 现有判定只看 status，给它们加到期
// 就得改那几条判定，而那是真实付费用户的计费路径。

// ProviderAdmin 标记后台手动授予的订阅。
//
// 不进 knownProviders：那张表校验的是外部输入，任何外部请求都不该能自称是后台。
const ProviderAdmin Provider = "admin"

// adminPeriodEnd 是手动行的 current_period_end。列是 NOT NULL，
// 填一个永远到不了的日子而不是 now()：将来若给 active 行补上到期判断，
// 填 now() 的手动行会在那一天全部静默失效。
var adminPeriodEnd = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

var (
	// ErrPaidPlanActive 用户有渠道付费的基础订阅仍在服务（含宽限期）。
	// 此时手动改档会把它顶掉，而渠道照常扣款、下一次续费 webhook 又把它写回来。
	ErrPaidPlanActive = errors.New("用户有付费中的渠道订阅，不能手动设置档位")
	// ErrNotAdminGrant 要撤销的行不存在、已失效，或不是手动授予的。
	ErrNotAdminGrant = errors.New("没有这条生效中的手动授予")
)

// Record 是一条仍在服务的订阅（active，或宽限期内的 past_due），供管理后台展示。
type Record struct {
	ID            uuid.UUID `json:"id"`
	Kind          string    `json:"kind"`
	Tier          string    `json:"tier"`
	StorageGB     *int      `json:"storage_gb,omitempty"`
	BillingPeriod string    `json:"billing_period"`
	Status        string    `json:"status"`
	Provider      Provider  `json:"provider"`
	PeriodEnd     time.Time `json:"current_period_end"`
	// GraceUntil 仅 past_due 时非空，与 wallet.PlanState 同义。
	GraceUntil *time.Time `json:"grace_until,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// stillLive 判断一行在 now 时是否仍在服务，并给 past_due 行补上宽限截止。
//
// 与 wallet.PlanState、quota.Storage 是同一条规则：active，或 past_due 且
// current_period_end + 宽限期 > now。那两处在 SQL 里判，这里取回行后在 Go 里判，
// 因为后台要的是行本身而不是一个聚合值；宽限期长度两边都取自 pricing.GraceFor。
func stillLive(r *Record, now time.Time) bool {
	switch r.Status {
	case "active":
		return true
	case "past_due":
		until := r.PeriodEnd.Add(pricing.GraceFor(r.BillingPeriod))
		if !until.After(now) {
			return false
		}
		r.GraceUntil = &until
		return true
	}
	return false
}

// ============ Repo ============

// Live 列出用户仍在服务的订阅，plan 在前，同类按创建时间排。
func (r *Repo) Live(ctx context.Context, userID uuid.UUID) ([]Record, error) {
	return live(ctx, r.pool, userID, time.Now())
}

func live(ctx context.Context, q querier, userID uuid.UUID, now time.Time) ([]Record, error) {
	rows, err := q.Query(ctx, `
		SELECT id, kind, tier, bonus_storage_gb, billing_period, status,
		       payment_provider, current_period_end, created_at
		FROM subscriptions
		WHERE user_id = $1 AND status IN ('active', 'past_due')
		ORDER BY (kind = 'plan') DESC, created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.Kind, &rec.Tier, &rec.StorageGB, &rec.BillingPeriod,
			&rec.Status, &rec.Provider, &rec.PeriodEnd, &rec.CreatedAt); err != nil {
			return nil, err
		}
		if stillLive(&rec, now) {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

// SetAdminPlan 把用户的手动档位换成 tier；tier 为 free 时只撤掉手动档位。
//
// 先让旧的手动 plan 退场再插新的，理由同 ActivatePlan。渠道付费的 plan 一概不碰，
// 有就拒绝，见 ErrPaidPlanActive。
func (r *Repo) SetAdminPlan(ctx context.Context, userID uuid.UUID, tier string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := userExists(ctx, tx, userID); err != nil {
		return err
	}
	recs, err := live(ctx, tx, userID, time.Now())
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Kind == "plan" && rec.Provider != ProviderAdmin {
			return ErrPaidPlanActive
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'expired', canceled_at = now(), updated_at = now()
		WHERE user_id = $1 AND kind = 'plan'
		  AND payment_provider = $2 AND status = 'active'`,
		userID, string(ProviderAdmin)); err != nil {
		return err
	}

	if tier != entitlement.Free {
		if _, err := insertAdmin(ctx, tx, userID, "plan", tier, nil); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// GrantAdminStorage 追加一份手动存储。与付费存储一样可叠加，配额取所有生效行之和。
func (r *Repo) GrantAdminStorage(ctx context.Context, userID uuid.UUID, tier string, gb int) (uuid.UUID, error) {
	if err := userExists(ctx, r.pool, userID); err != nil {
		return uuid.Nil, err
	}
	return insertAdmin(ctx, r.pool, userID, "storage", tier, &gb)
}

// RevokeAdmin 撤销一条生效中的手动授予。渠道付费的行不经这里，
// 它们的状态只认渠道的通知，后台改了也会被下一次 webhook 改回去。
func (r *Repo) RevokeAdmin(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'expired', canceled_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2
		  AND payment_provider = $3 AND status = 'active'`,
		id, userID, string(ProviderAdmin))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAdminGrant
	}
	return nil
}

// insertAdmin 落一条手动行。billing_period 填 monthly 只为满足 CHECK：
// 手动行不计费，也就没有周期；它唯一被读到的地方是 past_due 宽限期，而手动行永远不会 past_due。
func insertAdmin(ctx context.Context, q querier, userID uuid.UUID, kind, tier string, gb *int) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO subscriptions (
			user_id, kind, tier, bonus_storage_gb, billing_period, status,
			current_period_start, current_period_end, payment_provider)
		VALUES ($1, $2, $3, $4, 'monthly', 'active', now(), $5, $6)
		RETURNING id`,
		userID, kind, tier, gb, adminPeriodEnd, string(ProviderAdmin),
	).Scan(&id)
	return id, err
}

// userExists 挡住不存在或已软删除的用户。外键只挡前者 ——
// 给软删除的账号开档位不会报错，只会留下一条没人看得见的行。
func userExists(ctx context.Context, q querier, userID uuid.UUID) error {
	var ok bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnknownUser
	}
	return nil
}

// ============ Service ============

// Live 列出用户仍在服务的订阅。供管理后台的用户列表使用。
func (s *Service) Live(ctx context.Context, userID uuid.UUID) ([]Record, error) {
	return s.repo.Live(ctx, userID)
}

// SetAdminPlan 手动设置基础档位；free 表示撤掉手动档位。
func (s *Service) SetAdminPlan(ctx context.Context, userID uuid.UUID, tier string) error {
	if !entitlement.Known(tier) {
		return ErrUnknownTier
	}
	return s.repo.SetAdminPlan(ctx, userID, tier)
}

// GrantAdminStorage 手动追加一份存储。认目录里的全部档位，
// 包括只在 App 内售卖的小档 —— NativeOnly 限制的是售卖通路，不是档位本身。
func (s *Service) GrantAdminStorage(ctx context.Context, userID uuid.UUID, tier string) (uuid.UUID, error) {
	t, ok := pricing.StorageTierByKey(tier)
	if !ok {
		return uuid.Nil, ErrUnknownTier
	}
	return s.repo.GrantAdminStorage(ctx, userID, tier, t.GB)
}

// RevokeAdmin 撤销一条手动授予。
func (s *Service) RevokeAdmin(ctx context.Context, userID, id uuid.UUID) error {
	return s.repo.RevokeAdmin(ctx, userID, id)
}

// ============ Handler ============

// AdminHandler 是手动授予的内部端，挂在 /internal/admin 下。
type AdminHandler struct {
	svc *Service
}

func NewAdminHandler(svc *Service) *AdminHandler { return &AdminHandler{svc: svc} }

// RegisterInternal 受 X-Internal-Token 保护，供管理后台调用。
//
//	PUT    /users/:id/plan                    {"tier":"premium"}  设置手动档位；free = 撤掉
//	POST   /users/:id/storage                 {"tier":"s40"}      追加一份手动存储
//	DELETE /users/:id/subscriptions/:sub_id                       撤销一条手动授予
func (h *AdminHandler) RegisterInternal(g *gin.RouterGroup) {
	g.PUT("/users/:id/plan", h.setPlan)
	g.POST("/users/:id/storage", h.grantStorage)
	g.DELETE("/users/:id/subscriptions/:sub_id", h.revoke)
}

type adminTierInput struct {
	Tier string `json:"tier" binding:"required"`
}

func (h *AdminHandler) setPlan(c *gin.Context) {
	userID, ok := uuidParam(c, "id")
	if !ok {
		return
	}
	var in adminTierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.SetAdminPlan(c.Request.Context(), userID, in.Tier); err != nil {
		c.JSON(adminStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AdminHandler) grantStorage(c *gin.Context) {
	userID, ok := uuidParam(c, "id")
	if !ok {
		return
	}
	var in adminTierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id, err := h.svc.GrantAdminStorage(c.Request.Context(), userID, in.Tier)
	if err != nil {
		c.JSON(adminStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id})
}

func (h *AdminHandler) revoke(c *gin.Context) {
	userID, ok := uuidParam(c, "id")
	if !ok {
		return
	}
	subID, ok := uuidParam(c, "sub_id")
	if !ok {
		return
	}
	if err := h.svc.RevokeAdmin(c.Request.Context(), userID, subID); err != nil {
		c.JSON(adminStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func uuidParam(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return uuid.Nil, false
	}
	return id, true
}

func adminStatus(err error) int {
	switch {
	case errors.Is(err, ErrUnknownTier):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnknownUser), errors.Is(err, ErrNotAdminGrant):
		return http.StatusNotFound
	case errors.Is(err, ErrPaidPlanActive):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
