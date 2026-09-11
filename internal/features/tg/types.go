package tg

import (
	"encoding/json"
	"errors"
	"strconv"
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

// PlatformTelegram 是本包唯一签发/核销的平台标识。写成常量而不是散落的字符串
// 字面量，是因为接入第二个平台时，编译器能替你找出所有该分叉的地方。
const PlatformTelegram = "telegram"

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

// Binding 是面向用户的绑定记录，字段一律用平台无关的名字。
//
// 与 /internal/tg 上那套 `tg_user_id` 形状的分界是有意的：内部端点是 Bot 与 API
// 之间的私有协议，说 Telegram 的母语（int64 的 tg_user_id）才自然；而这个结构
// 出现在「我绑了哪些账号」这张用户可见的列表里，将来微信等平台的记录要与它并排，
// 所以它必须先是平台无关的。
//
// PlatformUserID 是字符串而不是 int64：Telegram 的 ID 恰好是数字，微信 openid
// 不是。让它在最外层就是字符串，比日后再改一次已发布的契约便宜。
type Binding struct {
	ID             uuid.UUID `json:"id"`
	Platform       string    `json:"platform"`
	PlatformUserID string    `json:"platform_user_id"`
	Username       *string   `json:"platform_username,omitempty"`
	Name           *string   `json:"platform_name,omitempty"`
	UserID         uuid.UUID `json:"user_id"`
	DeviceID       uuid.UUID `json:"device_id"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at"`
}

// TGIdentity 是 Bot 观察到的 TG 用户身份，核销令牌建立绑定时一并带上，
// 仅用于 App 端展示（「你绑的是哪个号」）。TG 侧改名不回流。
type TGIdentity struct {
	TGUserID int64  `json:"tg_user_id" binding:"required"`
	Username string `json:"tg_username,omitempty"`
	Name     string `json:"tg_name,omitempty"`
}

// identity 把 Bot 报上来的 Telegram 原生身份翻译成存储层的平台无关形状。
// 这是整个包里唯一一处 int64 → string 的转换点。
func (id TGIdentity) identity() Identity {
	return Identity{
		Platform: PlatformTelegram,
		UserID:   strconv.FormatInt(id.TGUserID, 10),
		Username: id.Username,
		Name:     id.Name,
	}
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
