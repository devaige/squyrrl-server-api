package tg

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCodeInvalid     = errors.New("绑定码无效或已过期")
	ErrAlreadyBound    = errors.New("该 Telegram 账号已绑定其它 Squyrrl 用户")
	ErrNotBound        = errors.New("Telegram 账号未绑定")
	ErrBindingNotFound = errors.New("绑定不存在")
)

// 绑定码长度（去掉易混淆字符的 base32 变体）
const BindingCodeLen = 8

type BindingCode struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Binding struct {
	ID         uuid.UUID `json:"id"`
	TGUserID   int64     `json:"tg_user_id"`
	TGUsername *string   `json:"tg_username,omitempty"`
	TGName     *string   `json:"tg_name,omitempty"`
	UserID     uuid.UUID `json:"user_id"`
	DeviceID   uuid.UUID `json:"device_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// TGIdentity 是 Bot 观察到的 TG 用户身份，签发绑定码与建立绑定时一并带上，
// 仅用于 App 端展示（「你绑的是哪个号」）。TG 侧改名不回流。
type TGIdentity struct {
	TGUserID int64  `json:"tg_user_id" binding:"required"`
	Username string `json:"tg_username,omitempty"`
	Name     string `json:"tg_name,omitempty"`
}

// =============================================================================
// 用户端（Bearer）
// =============================================================================

type RedeemInput struct {
	Code string `json:"code" binding:"required"`
}

// =============================================================================
// 内部端（X-Internal-Token，仅 Bot）
// =============================================================================

type IssueCodeInput struct {
	TGIdentity
}

type RevokeInput struct {
	TGUserID int64 `json:"tg_user_id" binding:"required"`
}

type BindingStatusResponse struct {
	Bound      bool   `json:"bound"`
	TGUsername string `json:"tg_username,omitempty"`
}

// SnippetForwardInput 是 Bot 收到可解析消息后调内部端点的载荷。
// 字段是 features/snippet.CreateInput 的子集：Bot 不该知道 page/tag/版本这些概念。
type SnippetForwardInput struct {
	TGUserID int64                 `json:"tg_user_id" binding:"required"`
	Snippet  ForwardedSnippetInput `json:"snippet"    binding:"required"`
}

type ForwardedSnippetInput struct {
	Type        string          `json:"type"` // 默认 normal
	Subtype     string          `json:"subtype,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	TextContent []byte          `json:"text_content,omitempty"`
	TextFormat  string          `json:"text_format,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	// 已经过 /internal/tg/files 上传拿到的 file_id，按顺序挂成 elements
	FileIDs []uuid.UUID `json:"file_ids,omitempty"`
	// TG 侧的消息坐标，写进 source_data 供追溯
	ChatID    int64 `json:"chat_id,omitempty"`
	MessageID int64 `json:"message_id,omitempty"`
}
