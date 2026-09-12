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

// RecordFailedOTPAttempt 记一次验证失败，并销毁已经用尽尝试次数的验证码。
// 返回本次被销毁的条数（>0 意味着有人在猜，是需要被看见的信号）。
//
// **计数是按邮箱而非按行**：冷却窗口 60s、TTL 10min，同一邮箱最多可有 10 个码同时有效，
// 若各自独立计数，攻击者就拿到了 10 × maxAttempts 次机会。所以一次失败让该邮箱下**全部**
// 存活的码各加一次 —— 无论攻击者当时想猜哪一个，总预算恒为 maxAttempts。
//
// **必须是单条 UPDATE，不能 SELECT 出来再改**：并发的两次错误猜测下，
// `attempts = attempts + 1` 由 Postgres 的行锁串行化，第二个事务会在锁释放后重读到
// 已提交的新值；先读后写则两者都基于同一个旧值，计数会丢。
//
// **也不能带 SKIP LOCKED**（隔壁 ConsumeEmailOTP 有）：那里跳过是为了让并发核销互不阻塞，
// 而这里跳过等于漏计——攻击者只要把请求打并发就能把计数器绕过去。阻塞才是对的。
func (r *Repo) RecordFailedOTPAttempt(ctx context.Context, email, purpose string, maxAttempts int) (int, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE email_otps
		SET attempts    = attempts + 1,
		    consumed_at = CASE WHEN attempts + 1 >= $3 THEN now() ELSE NULL END
		WHERE email = $1
		  AND purpose = $2
		  AND consumed_at IS NULL
		  AND expires_at > now()
		RETURNING consumed_at IS NOT NULL`,
		email, purpose, maxAttempts)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	burned := 0
	for rows.Next() {
		var isBurned bool
		if err := rows.Scan(&isBurned); err != nil {
			return 0, err
		}
		if isBurned {
			burned++
		}
	}
	return burned, rows.Err()
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

// UpsertDevice 同名同平台的未撤销设备复用记录；否则新建。
// 第二个返回值标明是否**新建**了一台 —— 只有新建才可能让用户越过档位上限，
// 复用一台老设备不改变设备总数，没必要每次登录都去跑一遍逐出。
func (r *Repo) UpsertDevice(ctx context.Context, userID uuid.UUID, name, platform string) (*Device, bool, error) {
	var d Device
	err := r.pool.QueryRow(ctx, `
		UPDATE devices
		SET last_seen_at = now()
		WHERE user_id = $1 AND name = $2 AND platform = $3 AND revoked_at IS NULL
		RETURNING id, user_id, name, platform, last_seen_at`,
		userID, name, platform,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.LastSeenAt)
	if err == nil {
		return &d, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if err := r.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, platform)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, name, platform, last_seen_at`,
		userID, name, platform,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.LastSeenAt); err != nil {
		return nil, false, err
	}
	return &d, true, nil
}

// EvictDevicesBeyond 保留最近活跃的 keep 台设备，其余撤销，返回被撤销的数量。
//
// 按 last_seen_at 逐出最久未活跃的那台，而不是最早注册的：用户想保住的是他
// 正在用的设备，注册时间早晚与此无关 —— 一台三年前注册、每天都在用的电脑
// 不该被今天新登录的手机挤掉。
//
// 排除三方绑定占位（platform_bindings 指向的那些）：它们由每平台绑定数管着，
// 两条门槛计同一样东西会让绑一个 TG 号白白吃掉一个登录名额。
func (r *Repo) EvictDevicesBeyond(ctx context.Context, userID uuid.UUID, keep int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE devices SET revoked_at = now()
		WHERE id IN (
			SELECT d.id FROM devices d
			WHERE d.user_id = $1 AND d.revoked_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM platform_bindings pb WHERE pb.device_id = d.id)
			ORDER BY d.last_seen_at DESC
			OFFSET $2
		)`, userID, keep)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
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
	// 连 devices 一起判：设备被撤销即视为会话失效。
	//
	// 这样「设备被踢」只需要写一处状态（devices.revoked_at），而不必同时去
	// 撤销它名下的会话 —— 两份状态总有一天会不同步，而不同步的表现是一台
	// 已经被踢掉的设备仍然能正常读写。代价是每次鉴权多一次主键查找。
	//
	// 逐出本身是**惰性生效**的（用户决策 2026-09-11）：不推送、不断连，
	// 被踢的那台在下一次请求时拿到 401，客户端既有的 onUnauthorized 会清掉登录态。
	q := `
		SELECT s.id, s.user_id, s.device_id, s.access_expires_at, s.refresh_expires_at
		FROM sessions s
		JOIN devices d ON d.id = s.device_id AND d.revoked_at IS NULL
		WHERE s.` + hashCol + ` = $1
		  AND s.revoked_at IS NULL
		  AND s.` + expCol + ` > now()`
	err := r.pool.QueryRow(ctx, q, hash).Scan(
		&s.ID, &s.UserID, &s.DeviceID, &s.AccessExpiresAt, &s.RefreshExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// RefreshSession 旋转 access 并把 refresh 的到期时间顺延（滑动过期）。
//
// refresh 令牌本身**不换**（ADR-019 的不轮换决定不变，理由仍是并发刷新互相失效的竞态），
// 变的只是 refresh_expires_at 的锚点：原先锚在签发时刻，于是任何会话满 30 天必死，
// 天天在用的用户也要重新走一遍邮件验证码。轮换与顺延是两件事，这里只动后者。
func (r *Repo) RefreshSession(
	ctx context.Context,
	sessionID uuid.UUID,
	accessHash []byte,
	accessExp, refreshExp time.Time,
) error {
	// refresh_expires_at 用 GREATEST 而不是直接赋值：两次并发刷新中先到的那次可能
	// 算出更晚的时间戳，后到的那次若无条件覆盖就会把到期时间**往回拨**。
	// 差值只有毫秒级，但这类回拨正是没人能复现的「偶尔被登出」。
	_, err := r.pool.Exec(ctx, `
		UPDATE sessions
		SET access_token_hash = $1,
		    access_expires_at = $2,
		    refresh_expires_at = GREATEST(refresh_expires_at, $3::timestamptz)
		WHERE id = $4`,
		accessHash, accessExp, refreshExp, sessionID)
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
