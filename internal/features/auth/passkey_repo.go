package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 简化模型：把 webauthn.Credential 整体 JSON-序列化存到 passkeys.public_key 字段。
// sign_count 单独抽出来便于诊断查询；写库时以 JSON 为准。

// ListUserCredentials 加载该用户全部 webauthn.Credential
func (r *Repo) ListUserCredentials(ctx context.Context, userID uuid.UUID) ([]webauthn.Credential, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT public_key, sign_count
		FROM passkeys
		WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []webauthn.Credential
	for rows.Next() {
		var blob []byte
		var signCount int64
		if err := rows.Scan(&blob, &signCount); err != nil {
			return nil, err
		}
		var cred webauthn.Credential
		if err := json.Unmarshal(blob, &cred); err != nil {
			return nil, err
		}
		// 以 DB 单独列为准（防止 JSON 漂移）
		cred.Authenticator.SignCount = uint32(signCount)
		out = append(out, cred)
	}
	return out, rows.Err()
}

// SavePasskey 把一条 webauthn.Credential 落库
func (r *Repo) SavePasskey(ctx context.Context, userID uuid.UUID, cred *webauthn.Credential, name string) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	transports := transportsToStrings(cred.Transport)

	var aaguidArg any
	if len(cred.Authenticator.AAGUID) == 16 {
		if u, err := uuid.FromBytes(cred.Authenticator.AAGUID); err == nil {
			aaguidArg = u
		}
	}

	_, err = r.pool.Exec(ctx, `
		INSERT INTO passkeys (user_id, credential_id, public_key, sign_count, transports, aaguid, name)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		userID, cred.ID, blob, int64(cred.Authenticator.SignCount), transports, aaguidArg, nilIfEmpty(name))
	return err
}

// FindUserByCredentialID 用 assertion 中的 raw credentialID 找出归属用户
func (r *Repo) FindUserByCredentialID(ctx context.Context, credID []byte) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.handle, u.created_at, u.updated_at
		FROM passkeys p JOIN users u ON u.id = p.user_id
		WHERE p.credential_id = $1 AND u.deleted_at IS NULL`,
		credID).Scan(&u.ID, &u.Email, &u.Handle, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// UpdatePasskeySignCount 在 ValidateLogin 后把新的 sign_count 写回
func (r *Repo) UpdatePasskeySignCount(ctx context.Context, credID []byte, signCount uint32) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE passkeys
		SET sign_count = $1, last_used_at = $2
		WHERE credential_id = $3`,
		int64(signCount), time.Now(), credID)
	return err
}

func transportsToStrings(t []protocol.AuthenticatorTransport) []string {
	out := make([]string, len(t))
	for i, x := range t {
		out[i] = string(x)
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
