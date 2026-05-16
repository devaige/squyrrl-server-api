package auth

import (
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// passkeyUser 把 Squyrrl 的 User + 已存凭据列表适配到 webauthn.User 接口
// （webauthn 库的注册 / 登录 ceremony 都通过这个接口取数据）
type passkeyUser struct {
	user        *User
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte {
	b, _ := u.user.ID.MarshalBinary()
	return b
}

func (u *passkeyUser) WebAuthnName() string {
	if u.user.Email != nil && *u.user.Email != "" {
		return *u.user.Email
	}
	return u.user.ID.String()
}

func (u *passkeyUser) WebAuthnDisplayName() string {
	if u.user.Handle != nil && *u.user.Handle != "" {
		return *u.user.Handle
	}
	return u.WebAuthnName()
}

func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential {
	return u.credentials
}

// userIDFromHandle 反向：把 WebAuthnID() 字节切回 uuid.UUID
func userIDFromHandle(b []byte) (uuid.UUID, error) {
	return uuid.FromBytes(b)
}
