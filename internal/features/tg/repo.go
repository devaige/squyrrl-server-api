package tg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// =============================================================================
// 绑定令牌：App 为已登录用户签发 → Bot 在 /start <token> 时核销
// =============================================================================

// IssueToken 先找该用户手上未核销的令牌，有就复用。
// 绑定页每次重建都会请求一次，不复用的话二维码会在用户正扫的时候换掉；
// 复用让令牌在 TTL 内是稳定的一张。
func (r *Repo) IssueToken(ctx context.Context, userID uuid.UUID, platform string, ttl time.Duration) (string, time.Time, error) {
	var token string
	var exp time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT token, expires_at FROM binding_tokens
		WHERE user_id = $1 AND platform = $2 AND consumed_at IS NULL AND expires_at > now()
		ORDER BY expires_at DESC LIMIT 1`, userID, platform).Scan(&token, &exp)
	if err == nil {
		return token, exp, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, err
	}

	token, err = generateToken()
	if err != nil {
		return "", time.Time{}, err
	}
	exp = time.Now().Add(ttl)
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO binding_tokens (token, user_id, platform, expires_at)
		VALUES ($1, $2, $3, $4)`, token, userID, platform, exp); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
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
