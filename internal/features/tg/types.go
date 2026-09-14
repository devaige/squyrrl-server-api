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

	// ErrCodeInvalid 同时代表「码不存在 / 已过期 / 已核销 / 正被另一个账号占着」。
	// 这四种刻意不分开：分开回答等于告诉猜码的人哪些码是存在的。
	ErrCodeInvalid = errors.New("绑定码无效或已过期")
	// ErrNoPendingClaim 同样合流了「没有待确认」与「待确认的不是这个账号」，
	// 因为用户的下一步动作相同：回 Bot 重发一次码。
	ErrNoPendingClaim  = errors.New("没有待确认的绑定申请")
	ErrTooManyAttempts = errors.New("尝试过于频繁，请稍后再试")
)

// PlatformTelegram 是本包唯一签发/核销的平台标识。写成常量而不是散落的字符串
// 字面量，是因为接入第二个平台时，编译器能替你找出所有该分叉的地方。
const PlatformTelegram = "telegram"

// BindingTokenBytes 是令牌的随机字节数。base64url 编码后 43 字符，
// 落在 Telegram deep link payload 的 64 字符上限与 [A-Za-z0-9_-] 字符集内。
const BindingTokenBytes = 32

// BindingLink 是 App 侧展示的绑定入口。同一枚令牌有三种搬运方式，覆盖
// 「App 所在设备」与「Telegram 所在设备」的全部组合：
//
//	URL       同设备装了 TG → 点一下直接唤起；另一台设备有摄像头 → 扫二维码
//	ShortCode 两者都不成立（典型：App 在手机、TG 在电脑）→ 人把 8 位码敲进 Bot
//
// Token 单独回传是历史契约，客户端不再直接用它。
type BindingLink struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ShortCode string    `json:"short_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PendingClaim 是「有个 TG 账号拿着你的短码来申请绑定，等你点头」。
//
// 它存在的全部意义是让短码不必是持票凭证：拿到码只能走到这一步，
// 而这一步会把申请者的身份摆到账户主人眼前。字段用平台无关的名字，
// 与 Binding 对齐 —— 这张确认框将来要同样服务于微信等平台。
type PendingClaim struct {
	Platform       string    `json:"platform"`
	PlatformUserID string    `json:"platform_user_id"`
	Username       string    `json:"platform_username,omitempty"`
	Name           string    `json:"platform_name,omitempty"`
	ClaimedAt      time.Time `json:"claimed_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// ConfirmClaimInput 用户端确认待绑定的账号。
//
// PlatformUserID 必填且服务端会比对：App 展示的是**它读到的那个** @username，
// 用户点头针对的也是那一个。不带这个字段的话，确认与绑定之间的空档里若换了一个
// 申请者，用户看着 A 点确认、绑上的是 B —— 而这恰恰是整个确认步骤要防的事。
type ConfirmClaimInput struct {
	PlatformUserID string `json:"platform_user_id" binding:"required"`
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

// ClaimCodeInput Bot 收到一条形如绑定码的私聊消息后提交：登记「这个 TG 号想绑
// 这枚令牌」，但**不建立绑定**。与 ConsumeTokenInput 的差别就是这一点，
// 也是短码敢做成人眼可读的全部依据。
type ClaimCodeInput struct {
	Code string `json:"code" binding:"required"`
	TGIdentity
}

// ClaimCodeResponse 回传令牌的过期时刻，Bot 据此决定等确认要轮询多久。
type ClaimCodeResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
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
