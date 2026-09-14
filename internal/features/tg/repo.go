package tg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity 是「某个平台上的某个账号」，是 repo 层唯一认识的身份形状。
//
// 与 TGIdentity 的分工：后者是 Bot 报上来的 Telegram 原生身份（`tg_user_id` 是
// int64），只活在 /internal/tg 那条边界上；跨过 Service 之后一律转成这里的
// 平台无关形状。存储层不该知道 Telegram 的 ID 恰好是个数字 —— 微信 openid 不是。
type Identity struct {
	Platform string
	UserID   string
	Username string
	Name     string
}

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// generateToken 产出 base64url 无填充的随机串。选这个编码不是为了紧凑，
// 而是因为它的字符集恰好是 Telegram deep link payload 允许的 [A-Za-z0-9_-]，
// 拼进 ?start= 不需要任何转义；标准 base64 的 +/= 会被 TG 截断或拒绝。
func generateToken() (string, error) {
	buf := make([]byte, BindingTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// shortCodeAlphabet 与 auth 的 QR 登录码用同一套字符集，刻意不另立门户：
// 两处都是「屏幕上显示、人照着敲」，排除 I/L/O/0/1 的理由完全相同，
// 而两套相似但不同的字符集只会让排错的人多一次核对。
const shortCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const shortCodeLen = 8

func generateShortCode() (string, error) {
	buf := make([]byte, shortCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, shortCodeLen)
	for i, b := range buf {
		out[i] = shortCodeAlphabet[int(b)%len(shortCodeAlphabet)]
	}
	return string(out), nil
}

// =============================================================================
// 绑定令牌：App 为已登录用户签发 → Bot 在 /start <token> 时核销
// =============================================================================

// issuedToken 是 IssueToken 的返回形状。用结构体而非四个返回值，是因为
// token 与 short_code 都是字符串，位置写反了编译器一句话都不会说。
type issuedToken struct {
	Token     string
	ShortCode string
	ExpiresAt time.Time
}

// IssueToken 先找该用户手上未核销的令牌，有就复用。
// 绑定页每次重建都会请求一次，不复用的话二维码会在用户正扫的时候换掉；
// 复用让令牌在 TTL 内是稳定的一张。
//
// 复用条件多了一个 short_code IS NOT NULL：000022 之前签发的令牌没有短码，
// 复用它会让绑定页少掉一条搬运路径。这类行最多存活 5 分钟（TTL），
// 让它们自然过期比写一次回填迁移便宜。
func (r *Repo) IssueToken(ctx context.Context, userID uuid.UUID, platform string, ttl time.Duration) (*issuedToken, error) {
	// 顺手清掉这个用户自己那些过期未核销的令牌。短码的唯一索引只按 consumed_at
	// 筛，不清理的话每个开过绑定页却没绑成的用户都会永久占着一个码名。
	// 按 user_id 收窄，走的是既有的 idx_binding_tokens_live。
	_, _ = r.pool.Exec(ctx, `
		DELETE FROM binding_tokens
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL AND expires_at <= now()`,
		userID, platform)

	var out issuedToken
	err := r.pool.QueryRow(ctx, `
		SELECT token, short_code, expires_at FROM binding_tokens
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL
		  AND expires_at > now() AND short_code IS NOT NULL
		ORDER BY expires_at DESC LIMIT 1`, userID, platform).
		Scan(&out.Token, &out.ShortCode, &out.ExpiresAt)
	if err == nil {
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(ttl)

	// 短码只有 31^8 个，唯一索引理论上会撞。重试而非一次性放弃：撞一次的概率是
	// 「当前活跃码数 / 8.5e11」，重试三次之后仍撞的概率不值得再写代码去处理。
	for attempt := 0; attempt < 3; attempt++ {
		code, err := generateShortCode()
		if err != nil {
			return nil, err
		}
		_, err = r.pool.Exec(ctx, `
			INSERT INTO binding_tokens (token, user_id, platform, expires_at, short_code)
			VALUES ($1, $2, $3, $4, $5)`, token, userID, platform, exp, code)
		if err == nil {
			return &issuedToken{Token: token, ShortCode: code, ExpiresAt: exp}, nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgErrUniqueViolation {
			return nil, err
		}
	}
	return nil, errors.New("绑定码签发失败：短码空间冲突")
}

// pgErrUniqueViolation 是 PostgreSQL 的 23505。
const pgErrUniqueViolation = "23505"

// ClaimToken 登记「某个平台账号想兑换这枚短码」，但不建立绑定。
//
// 判定条件里的最后一项是要害：已被 A 认领的码，B 再来认领会被拒绝（409），
// 而不是把 claim 覆盖掉。否则攻击者只要在受害者点「确认」的瞬间抢一次认领，
// 就能让那次点头落到自己头上 —— 确认框显示的身份与实际绑定的身份将不一致。
// 同一个账号重复发同一个码则是幂等的，用户手抖发两次不该报错。
func (r *Repo) ClaimToken(ctx context.Context, code, platform string, id Identity) (time.Time, error) {
	var exp time.Time
	err := r.pool.QueryRow(ctx, `
		UPDATE binding_tokens
		SET claimed_at = now(), claim_platform_user_id = $3,
		    claim_username = $4, claim_name = $5
		WHERE short_code = $1 AND platform = $2
		  AND consumed_at IS NULL AND expires_at > now()
		  AND (claimed_at IS NULL OR claim_platform_user_id = $3)
		RETURNING expires_at`,
		code, platform, id.UserID, nilIfEmpty(id.Username), nilIfEmpty(id.Name)).Scan(&exp)
	if errors.Is(err, pgx.ErrNoRows) {
		// 码不存在、已过期、已核销，与「被别人占着」在这里合流。分开回答等于
		// 告诉猜码的人「这个码是存在的」，而这条端点本来就是拿来猜的。
		return time.Time{}, ErrCodeInvalid
	}
	return exp, err
}

// FindPendingClaim 取该用户当前那枚令牌上待确认的申请，没有就 ErrNoPendingClaim。
func (r *Repo) FindPendingClaim(ctx context.Context, userID uuid.UUID, platform string) (*PendingClaim, error) {
	var p PendingClaim
	var username, name *string
	err := r.pool.QueryRow(ctx, `
		SELECT claim_platform_user_id, claim_username, claim_name, claimed_at, expires_at
		FROM binding_tokens
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL
		  AND expires_at > now() AND claimed_at IS NOT NULL
		ORDER BY claimed_at DESC LIMIT 1`, userID, platform).
		Scan(&p.PlatformUserID, &username, &name, &p.ClaimedAt, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoPendingClaim
	}
	if err != nil {
		return nil, err
	}
	p.Platform = platform
	p.Username, p.Name = deref(username), deref(name)
	return &p, nil
}

// ConsumeClaimedToken 核销一枚「已被指定账号认领」的令牌，返回该账号身份。
//
// expectPlatformUserID 参与 WHERE 而不是事后比对：核销与比对必须是同一条语句，
// 否则两者之间仍有空档。条件不满足时无从区分「没有待确认」与「待确认的不是这个」，
// 统一回 ErrNoPendingClaim —— 两种情况用户的下一步动作相同（回 Bot 重发一次码）。
func (r *Repo) ConsumeClaimedToken(
	ctx context.Context, userID uuid.UUID, platform, expectPlatformUserID string,
) (Identity, error) {
	var username, name *string
	err := r.pool.QueryRow(ctx, `
		UPDATE binding_tokens SET consumed_at = now()
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL
		  AND expires_at > now() AND claim_platform_user_id = $3
		RETURNING claim_username, claim_name`,
		userID, platform, expectPlatformUserID).Scan(&username, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrNoPendingClaim
	}
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Platform: platform,
		UserID:   expectPlatformUserID,
		Username: deref(username),
		Name:     deref(name),
	}, nil
}

// RejectClaim 用户拒绝了这次申请：整枚令牌作废（而不是只清 claim 字段）。
// 码已经到过陌生人手里，留着它继续可兑换等于让对方再试一次。
func (r *Repo) RejectClaim(ctx context.Context, userID uuid.UUID, platform string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE binding_tokens SET consumed_at = now()
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL AND claimed_at IS NOT NULL`,
		userID, platform)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoPendingClaim
	}
	return nil
}

// ConsumeToken 原子地核销一枚未过期未使用的令牌，返回它代表的 Squyrrl 用户。
// UPDATE ... RETURNING 一步完成判定与标记：两个 Bot 实例同时收到同一个
// token（用户连点两次链接）时，只有一个会拿到行。
// platform 参与判定而不只是记录：令牌在 App 里是为「绑定 Telegram」这个按钮签发的，
// 拿到另一个平台去核销就不该成立 —— 否则一枚截图外流的链接，能绑的平台由捡到它的人决定。
func (r *Repo) ConsumeToken(ctx context.Context, token, platform string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		UPDATE binding_tokens SET consumed_at = now()
		WHERE token = $1 AND platform = $2 AND consumed_at IS NULL AND expires_at > now()
		RETURNING user_id`, token, platform).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrTokenInvalid
	}
	return userID, err
}

// =============================================================================
// 绑定
// =============================================================================

// CreateBindingDevice 每个绑定的三方号一台「设备」：设备名带上该平台的身份，
// 用户在设备列表里才能分清绑了哪几个号。
func (r *Repo) CreateBindingDevice(ctx context.Context, userID uuid.UUID, platform, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, platform)
		VALUES ($1, $2, $3)
		RETURNING id`, userID, name, platform).Scan(&id)
	return id, err
}

// CountByPlatform 数某用户在某个平台上已绑的账号数，供档位门槛使用。
// 按平台分别计数是 BindingsPerPlatform 的字面含义：Basic 的 1 个是
// 「每个平台 1 个」，不是「所有平台合计 1 个」。
func (r *Repo) CountByPlatform(ctx context.Context, userID uuid.UUID, platform string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM platform_bindings WHERE user_id = $1 AND platform = $2`,
		userID, platform).Scan(&n)
	return n, err
}

// CreateBinding 落 (platform, platform_user_id) → (user_id, device_id)。
// ON CONFLICT DO NOTHING 而非 DO UPDATE：一个三方号只能属于一个 Squyrrl 账户，
// 想换账户必须先显式解绑。之前的 DO UPDATE 会让「拿到别人的码」变成一次静默改绑，
// 原账户毫无感知 —— 而 ErrAlreadyBound / 409 那条链路是为拒绝而写的，一直没生效。
func (r *Repo) CreateBinding(ctx context.Context, id Identity, userID, deviceID uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO platform_bindings (platform, platform_user_id, platform_username, platform_name, user_id, device_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (platform, platform_user_id) DO NOTHING`,
		id.Platform, id.UserID, nilIfEmpty(id.Username), nilIfEmpty(id.Name), userID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyBound
	}
	return nil
}

func (r *Repo) FindByAccount(ctx context.Context, platform, platformUserID string) (*Binding, error) {
	var b Binding
	err := r.pool.QueryRow(ctx, `
		SELECT id, platform, platform_user_id, platform_username, platform_name, user_id, device_id, created_at, last_used_at
		FROM platform_bindings WHERE platform = $1 AND platform_user_id = $2`, platform, platformUserID).
		Scan(&b.ID, &b.Platform, &b.PlatformUserID, &b.Username, &b.Name, &b.UserID, &b.DeviceID, &b.CreatedAt, &b.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotBound
	}
	return &b, err
}

// ListByUser 列出一个 Squyrrl 账户绑定的全部三方账号（跨平台，一对多）
func (r *Repo) ListByUser(ctx context.Context, userID uuid.UUID) ([]Binding, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, platform, platform_user_id, platform_username, platform_name, user_id, device_id, created_at, last_used_at
		FROM platform_bindings WHERE user_id = $1 ORDER BY platform, created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Binding, 0)
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.ID, &b.Platform, &b.PlatformUserID, &b.Username, &b.Name, &b.UserID, &b.DeviceID, &b.CreatedAt, &b.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteByID 用户端解绑：带 user_id 条件，避免越权删别人的绑定
func (r *Repo) DeleteByID(ctx context.Context, userID, bindingID uuid.UUID) (uuid.UUID, error) {
	var deviceID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		DELETE FROM platform_bindings WHERE id = $1 AND user_id = $2
		RETURNING device_id`, bindingID, userID).Scan(&deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrBindingNotFound
	}
	return deviceID, err
}

// DeleteByAccount 平台侧解绑（TG 的 /unbind）：Bot 只知道自己那侧的账号 ID
func (r *Repo) DeleteByAccount(ctx context.Context, platform, platformUserID string) (uuid.UUID, error) {
	var deviceID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		DELETE FROM platform_bindings WHERE platform = $1 AND platform_user_id = $2
		RETURNING device_id`, platform, platformUserID).Scan(&deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotBound
	}
	return deviceID, err
}

// RevokeDevice 解绑后把设备标记撤销，而不是删除：devices 被 snippets.source_data
// 之外的历史数据引用，硬删会牵动既有碎片的来源追溯。
func (r *Repo) RevokeDevice(ctx context.Context, deviceID uuid.UUID) {
	_, _ = r.pool.Exec(ctx,
		`UPDATE devices SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, deviceID)
}

func (r *Repo) TouchLastUsed(ctx context.Context, platform, platformUserID string) {
	_, _ = r.pool.Exec(ctx,
		`UPDATE platform_bindings SET last_used_at = now() WHERE platform = $1 AND platform_user_id = $2`,
		platform, platformUserID)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
