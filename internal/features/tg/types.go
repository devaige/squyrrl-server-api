package tg

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCodeInvalid    = errors.New("绑定码无效或已过期")
	ErrAlreadyBound   = errors.New("该 Telegram 账号已绑定其它 Squyrrl 用户")
	ErrNotBound       = errors.New("Telegram 账号未绑定")
)

// 绑定码长度（base32-without-confusing-chars，区分明显字符）
const BindingCodeLen = 8

type BindingCode struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Binding struct {
	ID         uuid.UUID `json:"id"`
	TGUserID   int64     `json:"tg_user_id"`
	UserID     uuid.UUID `json:"user_id"`
	DeviceID   uuid.UUID `json:"device_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// =============================================================================
// 内部 API（仅供 Bot 调用，受 X-Internal-Token 头保护）
// =============================================================================

type CompleteBindingInput struct {
	Code     string `json:"code"      binding:"required"`
	TGUserID int64  `json:"tg_user_id" binding:"required"`
}

type CompleteBindingResponse struct {
	UserID uuid.UUID `json:"user_id"`
}

// SnippetForwardInput 是 Bot 接收到转发消息后调内部端点的载荷。
// 字段子集化于 features/snippet.CreateInput，Bot 不需要知道全部细节。
type SnippetForwardInput struct {
	TGUserID int64                 `json:"tg_user_id" binding:"required"`
	Snippet  ForwardedSnippetInput `json:"snippet"    binding:"required"`
}

type ForwardedSnippetInput struct {
	Type        string `json:"type"`        // 默认 normal
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	TextContent []byte `json:"text_content,omitempty"`
	TextFormat  string `json:"text_format,omitempty"`
	// Bot 拿到的 source_data 由 server 端追加 platform/provider，无需 bot 传
}
