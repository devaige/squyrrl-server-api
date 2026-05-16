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

// 不易混淆字符集合：去掉 0/O、1/I/L
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func generateCode() (string, error) {
	buf := make([]byte, BindingCodeLen)
	bytes := make([]byte, BindingCodeLen)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	for i := 0; i < BindingCodeLen; i++ {
		buf[i] = codeAlphabet[int(bytes[i])%len(codeAlphabet)]
	}
	return string(buf), nil
}

// CreateOrReuseTelegramDevice 为该用户在 platform=telegram 下找已撤销前的设备，否则建一个新的
func (r *Repo) CreateOrReuseTelegramDevice(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		UPDATE devices SET last_seen_at = now()
		WHERE user_id = $1 AND platform = 'telegram' AND revoked_at IS NULL
		RETURNING id`, userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = r.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, platform)
		VALUES ($1, 'Telegram', 'telegram')
		RETURNING id`, userID).Scan(&id)
	return id, err
}

// IssueCode 生成绑定码并写表（10 分钟 TTL）
func (r *Repo) IssueCode(ctx context.Context, userID, deviceID uuid.UUID, ttl time.Duration) (*BindingCode, error) {
	code, err := generateCode()
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(ttl)
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO tg_binding_codes (code, user_id, device_id, expires_at)
		VALUES ($1, $2, $3, $4)`, code, userID, deviceID, exp); err != nil {
		return nil, err
	}
	return &BindingCode{Code: code, ExpiresAt: exp}, nil
}

// ConsumeCode 原子地查找并消费一条未过期未消费的绑定码
func (r *Repo) ConsumeCode(ctx context.Context, code string) (userID, deviceID uuid.UUID, err error) {
	err = r.pool.QueryRow(ctx, `
		UPDATE tg_binding_codes SET consumed_at = now()
		WHERE code = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING user_id, device_id`,
		code).Scan(&userID, &deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrCodeInvalid
	}
	return
}

// CreateBinding 落库 (tg_user_id, user_id, device_id) 三元组
// 若 tg_user_id 已绑其它用户，返回 ErrAlreadyBound
func (r *Repo) CreateBinding(ctx context.Context, tgUserID int64, userID, deviceID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tg_bindings (tg_user_id, user_id, device_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (tg_user_id) DO UPDATE
			SET user_id = EXCLUDED.user_id,
			    device_id = EXCLUDED.device_id,
			    last_used_at = now()`,
		tgUserID, userID, deviceID)
	return err
}

// FindByTGUser 找绑定记录；用于内部转发端点
func (r *Repo) FindByTGUser(ctx context.Context, tgUserID int64) (*Binding, error) {
	var b Binding
	err := r.pool.QueryRow(ctx, `
		SELECT id, tg_user_id, user_id, device_id, created_at, last_used_at
		FROM tg_bindings WHERE tg_user_id = $1`, tgUserID).
		Scan(&b.ID, &b.TGUserID, &b.UserID, &b.DeviceID, &b.CreatedAt, &b.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotBound
	}
	return &b, err
}

// TouchLastUsed 在转发消息时更新 last_used_at
func (r *Repo) TouchLastUsed(ctx context.Context, tgUserID int64) {
	_, _ = r.pool.Exec(ctx, `UPDATE tg_bindings SET last_used_at = now() WHERE tg_user_id = $1`, tgUserID)
}
