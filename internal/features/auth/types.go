package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// 哨兵错误：在 service / repo 层使用，handler 据此映射到合适的 HTTP 状态码
var (
	ErrNotFound           = errors.New("not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSessionRevoked     = errors.New("session revoked or expired")
)

// =============================================================================
// 领域类型
// =============================================================================

type User struct {
	ID        uuid.UUID `json:"id"`
	Email     *string   `json:"email,omitempty"`
	Handle    *string   `json:"handle,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Device struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"user_id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

type Session struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	DeviceID         uuid.UUID
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

// Identity 是经过认证后写入 Gin Context 的轻量身份；不直接暴露给外部
type Identity struct {
	UserID    uuid.UUID
	DeviceID  uuid.UUID
	SessionID uuid.UUID
}

// =============================================================================
// 请求 / 响应 DTO
// =============================================================================

type RequestEmailOTPInput struct {
	Email string `json:"email" binding:"required,email"`
}

type DeviceInput struct {
	Name     string `json:"name"     binding:"required,min=1,max=100"`
	Platform string `json:"platform" binding:"required,oneof=ios android macos windows linux web browser telegram wechat"`
}

type VerifyEmailOTPInput struct {
	Email  string      `json:"email"  binding:"required,email"`
	Code   string      `json:"code"   binding:"required,len=6"`
	Device DeviceInput `json:"device" binding:"required"`
}

type RefreshInput struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type QRCode struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RedeemQRInput struct {
	Code   string      `json:"code"   binding:"required,len=8"`
	Device DeviceInput `json:"device" binding:"required"`
}

type TokenPair struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type LoginResult struct {
	User   *User      `json:"user"`
	Device *Device    `json:"device"`
	Tokens *TokenPair `json:"tokens"`
}
