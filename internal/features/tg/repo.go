package tg

import (
	"context"
	"crypto/rand"
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

// 不易混淆字符集：去掉 0/O、1/I/L。用户要在 TG 里读出来、再敲进 App，
// 一个读错的字符就是一次失败的绑定。
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func generateCode() (string, error) {
	out := make([]byte, BindingCodeLen)
	buf := make([]byte, BindingCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := 0; i < BindingCodeLen; i++ {
		out[i] = codeAlphabet[int(buf[i])%len(codeAlphabet)]
	}
	return string(out), nil
}

// =============================================================================
// 绑定码：Bot 为 tg_user_id 签发 → 用户在 App 内兑换
// =============================================================================

// IssueCode 先找该 TG 用户手上未过期的码，有就复用。
// 未绑定用户每发一条消息都签发新码的话，他手里会攒下一串都还有效的码，
// 而 App 只能输一个 —— 复用让「码」在 TTL 内是稳定的一张。
func (r *Repo) IssueCode(ctx context.Context, id TGIdentity, ttl time.Duration) (*BindingCode, error) {
	var code string
	var exp time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT code, expires_at FROM tg_binding_codes
		WHERE tg_user_id = $1 AND consumed_at IS NULL AND expires_at > now()
		ORDER BY expires_at DESC LIMIT 1`, id.TGUserID).Scan(&code, &exp)
	if err == nil {
		return &BindingCode{Code: code, ExpiresAt: exp}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	code, err = generateCode()
	if err != nil {
		return nil, err
	}
	exp = time.Now().Add(ttl)
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO tg_binding_codes (code, tg_user_id, tg_username, tg_name, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		code, id.TGUserID, nilIfEmpty(id.Username), nilIfEmpty(id.Name), exp); err != nil {
		return nil, err
	}
	return &BindingCode{Code: code, ExpiresAt: exp}, nil
}

// ConsumeCode 原子地消费一条未过期未消费的码，返回它代表的 TG 身份
func (r *Repo) ConsumeCode(ctx context.Context, code string) (TGIdentity, error) {
	var id TGIdentity
	var username, name *string
	err := r.pool.QueryRow(ctx, `
		UPDATE tg_binding_codes SET consumed_at = now()
		WHERE code = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING tg_user_id, tg_username, tg_name`, code).
		Scan(&id.TGUserID, &username, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, ErrCodeInvalid
	}
	if err != nil {
		return id, err
	}
	id.Username, id.Name = deref(username), deref(name)
	return id, nil
}

// =============================================================================
// 绑定
// =============================================================================

// CreateTelegramDevice 每个 TG 号一台「设备」：设备名带上 TG 身份，
// 用户在设备列表里才能分清绑了哪几个号。
func (r *Repo) CreateTelegramDevice(ctx context.Context, userID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, platform)
		VALUES ($1, $2, 'telegram')
		RETURNING id`, userID, name).Scan(&id)
	return id, err
}

// CreateBinding 落 (tg_user_id, user_id, device_id)。
// ON CONFLICT DO NOTHING 而非 DO UPDATE：一个 TG 号只能属于一个 Squyrrl 账户，
// 想换账户必须先显式解绑。之前的 DO UPDATE 会让「拿到别人的码」变成一次静默改绑，
// 原账户毫无感知 —— 而 ErrAlreadyBound / 409 那条链路是为拒绝而写的，一直没生效。
func (r *Repo) CreateBinding(ctx context.Context, id TGIdentity, userID, deviceID uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO tg_bindings (tg_user_id, tg_username, tg_name, user_id, device_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tg_user_id) DO NOTHING`,
		id.TGUserID, nilIfEmpty(id.Username), nilIfEmpty(id.Name), userID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyBound
	}
	return nil
}

func (r *Repo) FindByTGUser(ctx context.Context, tgUserID int64) (*Binding, error) {
	var b Binding
	err := r.pool.QueryRow(ctx, `
		SELECT id, tg_user_id, tg_username, tg_name, user_id, device_id, created_at, last_used_at
		FROM tg_bindings WHERE tg_user_id = $1`, tgUserID).
		Scan(&b.ID, &b.TGUserID, &b.TGUsername, &b.TGName, &b.UserID, &b.DeviceID, &b.CreatedAt, &b.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotBound
	}
	return &b, err
}

// ListByUser 列出一个 Squyrrl 账户绑定的全部 TG 号（一对多）
func (r *Repo) ListByUser(ctx context.Context, userID uuid.UUID) ([]Binding, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tg_user_id, tg_username, tg_name, user_id, device_id, created_at, last_used_at
		FROM tg_bindings WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Binding, 0)
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.ID, &b.TGUserID, &b.TGUsername, &b.TGName,
			&b.UserID, &b.DeviceID, &b.CreatedAt, &b.LastUsedAt); err != nil {
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
		DELETE FROM tg_bindings WHERE id = $1 AND user_id = $2
		RETURNING device_id`, bindingID, userID).Scan(&deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrBindingNotFound
	}
	return deviceID, err
}

// DeleteByTGUser Bot 端解绑（/unbind）：TG 侧只知道自己的 tg_user_id
func (r *Repo) DeleteByTGUser(ctx context.Context, tgUserID int64) (uuid.UUID, error) {
	var deviceID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		DELETE FROM tg_bindings WHERE tg_user_id = $1
		RETURNING device_id`, tgUserID).Scan(&deviceID)
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

func (r *Repo) TouchLastUsed(ctx context.Context, tgUserID int64) {
	_, _ = r.pool.Exec(ctx, `UPDATE tg_bindings SET last_used_at = now() WHERE tg_user_id = $1`, tgUserID)
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
