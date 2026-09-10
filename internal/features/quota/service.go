package quota

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// PlanReader 提供用户当前档位。由 wallet.Service 实现 —— 用接口而不是直接依赖，
// 是为了让 quota 的单测不必拖进一个数据库连接池。
type PlanReader interface {
	ActivePlan(ctx context.Context, userID uuid.UUID) (string, error)
}

type Service struct {
	pool  *pgxpool.Pool
	plans PlanReader
}

func NewService(pool *pgxpool.Pool, plans PlanReader) *Service {
	return &Service{pool: pool, plans: plans}
}

// tierOf 取用户档位对应的能力表。
//
// 未知档位一律按 free 处理（MustOf 的语义）：档位字符串来自 subscriptions 表，
// 若某天出现一个代码不认识的值，宁可保守限制也不要放行 —— 放行的代价是成本泄漏，
// 而保守的代价只是一次用户投诉。
func (s *Service) tierOf(ctx context.Context, userID uuid.UUID) (entitlement.Tier, string, error) {
	plan, err := s.plans.ActivePlan(ctx, userID)
	if err != nil {
		return entitlement.Tier{}, "", err
	}
	return entitlement.MustOf(plan), plan, nil
}

// RequireSync 断言用户所在档位允许把数据同步到服务端。
//
// 这是所有门槛里最重要的一道，因为它直接对应服务端成本：免费档是纯本地应用
// （ADR-075），它的碎片压根不该出现在 Postgres 里。客户端已经在本地分支处理，
// 服务端这一层是防御 —— 客户端被改造或有人直接打 API 时仍然拦得住。
func (s *Service) RequireSync(ctx context.Context, userID uuid.UUID) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}
	return nil
}

// CheckSnippetCreate 在新建碎片前校验总量。
func (s *Service) CheckSnippetCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitSnippets,
		`SELECT count(*) FROM snippets WHERE user_id = $1 AND deleted_at IS NULL`)
}

// CheckPageCreate 在新建页面前校验总量。系统页面（冲突收件箱）不计入 ——
// 它由服务端按需自建，把它算进用户配额会导致「用户什么都没做却少了一个额度」。
func (s *Service) CheckPageCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitPages,
		`SELECT count(*) FROM pages WHERE user_id = $1 AND deleted_at IS NULL AND is_system = FALSE`)
}

// CheckTagCreate 在新建标签前校验总量。
func (s *Service) CheckTagCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitTags,
		`SELECT count(*) FROM tags WHERE user_id = $1`)
}

// CheckDeviceCreate 在注册新设备前校验数量。
func (s *Service) CheckDeviceCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitDevices,
		`SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL`)
}

// CheckHiddenPage 校验「隐藏页面」这项能力。它是布尔门槛，没有数量概念。
func (s *Service) CheckHiddenPage(ctx context.Context, userID uuid.UUID) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.HiddenPages {
		return newCapabilityError(LimitHiddenPages, plan, firstTierWith(func(x entitlement.Tier) bool {
			return x.HiddenPages
		}))
	}
	return nil
}

// checkCount 是数量型门槛的共同实现：取档位上限，count 当前用量，超了就拒。
//
// 每次 create 都多一次 count 查询是有代价的，但这些表都有 user_id 的部分索引，
// 而 create 本身也不是高频路径。真到了需要优化的那天，正确做法是维护计数器列
// 并定期对账，而不是取消检查 —— 但那属于过早优化，现在不做。
func (s *Service) checkCount(ctx context.Context, userID uuid.UUID, limit Limit, countSQL string) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}

	max := capOf(t, limit)
	var current int
	if err := s.pool.QueryRow(ctx, countSQL, userID).Scan(&current); err != nil {
		return err
	}
	if current >= max {
		return newLimitError(limit, plan, max, current)
	}
	return nil
}

// firstTierWith 返回第一个满足条件的档位键，用于布尔能力的「需要升到哪一档」。
func firstTierWith(pred func(entitlement.Tier) bool) string {
	for _, key := range entitlement.Order {
		if t, ok := entitlement.Of(key); ok && pred(t) {
			return key
		}
	}
	return ""
}

// StorageStatus 是用户的云存储配额与占用，单位统一为字节。
type StorageStatus struct {
	QuotaBytes int64 `json:"quota_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
}

// Storage 汇总用户的存储配额与实际占用。
//
// 配额来自所有 active 的 storage 订阅之和 —— 它们可以叠加（ADR-075），
// 所以这里是 SUM 而不是取最新一条。plan **不贡献任何配额**：三块商品互相独立，
// 免费档和至尊版自带的存储都是 0。
//
// 占用按「每个引用者全额计」口径（ADR-075 用户决策）：
// DISTINCT 在用户内部去重（同一用户的多条碎片引用同一文件只算一次），
// 但跨用户各算各的 —— 全局去重（ADR-026）省下的字节留在成本侧，不回馈给用户。
// 反过来做的话，别人删掉一个共享文件会让你的占用凭空上涨，那是没人能理解的账单。
func (s *Service) Storage(ctx context.Context, userID uuid.UUID) (StorageStatus, error) {
	var st StorageStatus

	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(bonus_storage_gb), 0)::bigint * 1024 * 1024 * 1024
		FROM subscriptions
		WHERE user_id = $1 AND kind = 'storage' AND status = 'active'`,
		userID).Scan(&st.QuotaBytes); err != nil {
		return st, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(f.size_bytes), 0)
		FROM (
			SELECT DISTINCT e.file_id
			FROM elements e
			JOIN snippets s ON s.id = e.snippet_id
			WHERE s.user_id = $1 AND s.deleted_at IS NULL AND e.file_id IS NOT NULL
		) uf
		JOIN files f ON f.id = uf.file_id`,
		userID).Scan(&st.UsedBytes); err != nil {
		return st, err
	}

	return st, nil
}
