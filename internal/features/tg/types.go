package tg

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTokenInvalid    = errors.New("绑定链接无效或已过期")
	ErrAlreadyBound    = errors.New("该 Telegram 账号已绑定其它 Squyrrl 用户")
	ErrNotBound        = errors.New("Telegram 账号未绑定")
	ErrBindingNotFound = errors.New("绑定不存在")
	ErrBotUnconfigured = errors.New("服务端未配置 Telegram Bot 用户名")
)

// BindingTokenBytes 是令牌的随机字节数。base64url 编码后 43 字符，
// 落在 Telegram deep link payload 的 64 字符上限与 [A-Za-z0-9_-] 字符集内。
const BindingTokenBytes = 32

// BindingLink 是 App 侧展示的绑定入口：URL 渲染成二维码 + 可点链接，
// Token 单独回传是为了在无法唤起 TG 的环境里给一个兜底的复制项。
type BindingLink struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
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

// TGIdentity 是 Bot 观察到的 TG 用户身份，核销令牌建立绑定时一并带上，
// 仅用于 App 端展示（「你绑的是哪个号」）。TG 侧改名不回流。
type TGIdentity struct {
	TGUserID int64  `json:"tg_user_id" binding:"required"`
	Username string `json:"tg_username,omitempty"`
	Name     string `json:"tg_name,omitempty"`
}

// =============================================================================
// 内部端（X-Internal-Token，仅 Bot）
// =============================================================================

// ConsumeTokenInput Bot 收到 /start <token> 后提交：令牌换 Squyrrl 账户，
// 连同它观察到的 TG 身份一起落成绑定。
type ConsumeTokenInput struct {
	Token string `json:"token" binding:"required"`
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
