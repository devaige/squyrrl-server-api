package auth

import (
	"context"
	cryptoRand "crypto/rand"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// =============================================================================
// email_otps
// =============================================================================

// CreateEmailOTP 写入一条新的 OTP 记录
func (r *Repo) CreateEmailOTP(ctx context.Context, email string, codeHash []byte, purpose string, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO email_otps (email, code_hash, purpose, expires_at)
		VALUES ($1, $2, $3, $4)`,
		email, codeHash, purpose, expiresAt)
	return err
}

// HasRecentOTP 报告该邮箱在 within 内是否已有一条未消费的 OTP（第 ① 层：按邮箱冷却）。
//
// 命中既有的 idx_email_otps_lookup(email, purpose, created_at DESC) WHERE consumed_at IS NULL，
// 无需新增索引 —— 那条部分索引本来就是为「查这个邮箱最近的未消费 OTP」建的。
func (r *Repo) HasRecentOTP(ctx context.Context, email, purpose string, within time.Duration) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM email_otps
			WHERE email = $1 AND purpose = $2
			  AND consumed_at IS NULL
			  AND created_at > now() - make_interval(secs => $3)
		)`, email, purpose, within.Seconds()).Scan(&exists)
	return exists, err
}

// CountOTPsSince 统计最近 window 内创建的 OTP 条数，作为全局发信预算的读数（第 ③ 层）。
//
// 用**滚动 24 小时**而不是自然日：自然日要先回答「谁的时区」（服务器？Resend 的配额按
// UTC 重置？），而滚动窗口对任何时区都成立，且严格更保守 —— 滚动 24h 内不超预算，
// 则任何自然日内也必不超。
//
// 刻意不为它加索引：这张表的规模恰恰被本预算本身限制住了（超预算就不再建行），
// 一年满打满算也就几万行，count 的顺序扫描是微秒级；为一个自我限幅的表加索引不划算。
func (r *Repo) CountOTPsSince(ctx context.Context, window time.Duration) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM email_otps
		WHERE created_at > now() - make_interval(secs => $1)`, window.Seconds()).Scan(&n)
	return n, err
}

// ConsumeEmailOTP 原子地查找并消费一条匹配的未过期 OTP
// 找不到（含 OTP 错误、已过期、已消费）一律返回 ErrNotFound
func (r *Repo) ConsumeEmailOTP(ctx context.Context, email string, codeHash []byte, purpose string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE email_otps
		SET consumed_at = now()
		WHERE id = (
			SELECT id FROM email_otps
			WHERE email = $1
			  AND code_hash = $2
			  AND purpose = $3
			  AND consumed_at IS NULL
			  AND expires_at > now()
			ORDER BY created_at DESC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)`,
		email, codeHash, purpose)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// =============================================================================
// users
// =============================================================================

// UpsertUserByEmail 按邮箱查找用户；不存在则创建
func (r *Repo) UpsertUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO users (email)
			VALUES ($1)
			ON CONFLICT (email) DO NOTHING
			RETURNING id, email, handle, created_at, updated_at
		)
		SELECT id, email, handle, created_at, updated_at FROM ins
		UNION ALL
		SELECT id, email, handle, created_at, updated_at FROM users WHERE email = $1
		LIMIT 1`,
		email,
	).Scan(&u.ID, &u.Email, &u.Handle, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// =============================================================================
// qr_login_codes
// =============================================================================

const qrCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // 同 tg：去掉易混淆字符
const qrCodeTTL = 90 * time.Second

func generateQRCode() (string, error) {
	buf := make([]byte, 8)
	bytes := make([]byte, 8)
	if _, err := cryptoRand.Read(bytes); err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		buf[i] = qrCodeAlphabet[int(bytes[i])%len(qrCodeAlphabet)]
	}
	return string(buf), nil
}

// IssueQRCode 给已登录用户签发一个 90 秒有效的绑定码
func (r *Repo) IssueQRCode(ctx context.Context, userID uuid.UUID) (*QRCode, error) {
	code, err := generateQRCode()
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(qrCodeTTL)
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO qr_login_codes (code, user_id, expires_at)
		VALUES ($1, $2, $3)`, code, userID, exp); err != nil {
		return nil, err
	}
	return &QRCode{Code: code, ExpiresAt: exp}, nil
}

// ConsumeQRCode 原子地兑换绑定码；过期或已消费均返 ErrNotFound
func (r *Repo) ConsumeQRCode(ctx context.Context, code string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		UPDATE qr_login_codes SET consumed_at = now()
		WHERE code = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING user_id`, code).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return userID, err
}

// =============================================================================
// users (续)
// =============================================================================

// GetUser 按 ID 拉用户；软删除的用户视为不存在
func (r *Repo) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		SELECT id, email, handle, created_at, updated_at FROM users
		WHERE id = $1 AND deleted_at IS NULL`,
		id,
	).Scan(&u.ID, &u.Email, &u.Handle, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// =============================================================================
// devices
// =============================================================================

// UpsertDevice 同名同平台的未撤销设备复用记录；否则新建
func (r *Repo) UpsertDevice(ctx context.Context, userID uuid.UUID, name, platform string) (*Device, error) {
	var d Device
	err := r.pool.QueryRow(ctx, `
		UPDATE devices
		SET last_seen_at = now()
		WHERE user_id = $1 AND name = $2 AND platform = $3 AND revoked_at IS NULL
		RETURNING id, user_id, name, platform, last_seen_at`,
		userID, name, platform,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = r.pool.QueryRow(ctx, `
			INSERT INTO devices (user_id, name, platform)
			VALUES ($1, $2, $3)
			RETURNING id, user_id, name, platform, last_seen_at`,
			userID, name, platform,
		).Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.LastSeenAt)
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// =============================================================================
// sessions
// =============================================================================

// CreateSession 创建会话；返回会话 ID
func (r *Repo) CreateSession(
	ctx context.Context,
	userID, deviceID uuid.UUID,
	accessHash, refreshHash []byte,
	accessExp, refreshExp time.Time,
) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, device_id, access_token_hash, refresh_token_hash, access_expires_at, refresh_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		userID, deviceID, accessHash, refreshHash, accessExp, refreshExp,
	).Scan(&id)
	return id, err
}

// FindSessionByAccessHash 用于鉴权中间件
func (r *Repo) FindSessionByAccessHash(ctx context.Context, hash []byte) (*Session, error) {
	return r.findSession(ctx, "access_token_hash", "access_expires_at", hash)
}

// FindSessionByRefreshHash 用于刷新流程
func (r *Repo) FindSessionByRefreshHash(ctx context.Context, hash []byte) (*Session, error) {
	return r.findSession(ctx, "refresh_token_hash", "refresh_expires_at", hash)
}

func (r *Repo) findSession(ctx context.Context, hashCol, expCol string, hash []byte) (*Session, error) {
	var s Session
	// 安全：hashCol/expCol 是包内常量字符串，永不来自外部输入
	q := `
		SELECT id, user_id, device_id, access_expires_at, refresh_expires_at
		FROM sessions
		WHERE ` + hashCol + ` = $1
		  AND revoked_at IS NULL
		  AND ` + expCol + ` > now()`
	err := r.pool.QueryRow(ctx, q, hash).Scan(
		&s.ID, &s.UserID, &s.DeviceID, &s.AccessExpiresAt, &s.RefreshExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// RotateAccessToken 仅旋转 access；refresh 维持不变
func (r *Repo) RotateAccessToken(ctx context.Context, sessionID uuid.UUID, accessHash []byte, accessExp time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE sessions
		SET access_token_hash = $1, access_expires_at = $2
		WHERE id = $3`,
		accessHash, accessExp, sessionID)
	return err
}

// RevokeSession 撤销会话（登出 / 设备超限被踢）
func (r *Repo) RevokeSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL`,
		sessionID)
	return err
}
