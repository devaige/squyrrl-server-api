package subscriptions

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnknownUser   = errors.New("subscription event references unknown user")
	ErrUnknownTier   = errors.New("subscription event has unknown tier")
	ErrBadSignature  = errors.New("webhook signature verification failed")
	ErrUnknownEvent  = errors.New("webhook event type not handled")
	ErrMalformedBody = errors.New("malformed webhook body")

	// 以下四个是客户端上报内购凭证时的拒绝理由。分开命名而不是共用一个
	// ErrBadSignature：这四种情况里凭证的**签名都是真的**，苹果确实签过它，
	// 差别在于「它不是这个 App 的 / 不是这个环境的 / 不是你的」。
	ErrAppleUnconfigured = errors.New("App Store 收单未配置")
	ErrWrongApp          = errors.New("凭证不属于本应用")
	ErrWrongEnvironment  = errors.New("凭证来自另一个购买环境")
	ErrForeignReceipt    = errors.New("凭证不属于当前登录账号")

	// Google Play 侧多出来的两个。它们存在的理由是 Play 的凭证不自证：
	// purchaseToken 只是个句柄，必须回源去问（见 play.go），于是「这笔单不成立」
	// 和「我们这会儿问不到」成了两种完全不同的失败。
	//
	// 分开命名不是为了措辞好看 —— 客户端对它们的处置**相反**：前者要把这笔
	// 交易了结掉（永远不会成立，留着只会占住这个商品），后者绝不能了结
	// （下次启动还要重报）。混成一个错误，二选一都会出事。
	ErrPlayUnconfigured = errors.New("Google Play 收单未配置")
	ErrUnknownPurchase  = errors.New("商店里查无此单")
	ErrStoreUnavailable = errors.New("商店暂时不可用")
)

// Provider 区分支付渠道。落库到 subscriptions.payment_provider 字段。
//
// 这张表是客户端 payment_channel.dart 里 PaymentChannel 枚举的**服务端镜像**，
// 两边必须同名同值：客户端按构建变体（area × market）决定走哪条通路，服务端按
// 这个值决定用哪套验签去收单。两边分家的表现是一笔已完成的支付落不了库。
type Provider string

const (
	ProviderStripe Provider = "stripe"
	ProviderApple  Provider = "app_store"
	ProviderGoogle Provider = "google_play"
	ProviderHuawei Provider = "huawei_iap"
	ProviderAlipay Provider = "alipay"
	ProviderWechat Provider = "wechat_pay"
)

// knownProviders 只用于校验外部输入里的 provider 串。
// 注意「已知」不等于「已接通」：后三家目前没有任何验签实现，收单入口会直接 501。
var knownProviders = map[Provider]bool{
	ProviderStripe: true, ProviderApple: true, ProviderGoogle: true,
	ProviderHuawei: true, ProviderAlipay: true, ProviderWechat: true,
}

func KnownProvider(p Provider) bool { return knownProviders[p] }

// SubscriptionEvent 是统一的内部事件结构，三家 webhook 各自映射到这里。
// 这样 service 层只关心「该激活 / 该续期 / 该取消」三种语义，与具体 provider 解耦。
type SubscriptionEvent struct {
	Provider               Provider
	ProviderSubscriptionID string    // Stripe sub_xxx / Apple originalTransactionId / Google purchaseToken
	UserID                 uuid.UUID // 必填：从 metadata / external account 取
	Kind                   string    // 'plan' | 'storage'
	Tier                   string    // 'basic'|'standard'|'premium'|'maximum' 或 storage tier
	BillingPeriod          string    // 'monthly' | 'yearly'
	Status                 string    // 'active' | 'past_due' | 'canceled' | 'expired'
	PeriodStart            time.Time
	PeriodEnd              time.Time
	CanceledAt             *time.Time
	BonusStorageGB         *int // storage 类型时填入
}

// OneTimePurchase 是一次性支付（代币加购）的内部事件。
//
// 与 SubscriptionEvent 分开而不是塞进去当一种 Kind：它们的生命周期完全不同 ——
// 订阅有状态机（active / past_due / canceled）、有周期、要 upsert 同一行；
// 一次性支付只有「发生过」这一个状态，落的是 credits_ledger 的一笔流水。
// 合并成一个结构，service 里会立刻长出一堆「这个字段对那种事件无意义」的分支。
type OneTimePurchase struct {
	Provider Provider
	// ProviderPaymentID 是幂等键的来源：支付网关重试是常态而非异常，
	// 同一笔支付到达两次必须只发一次币（credits_ledger 的部分唯一索引兜底）。
	ProviderPaymentID string
	UserID            uuid.UUID
	Credits           int64
}

// IdempotencyKey 供 wallet.Grant 去重。带 provider 前缀是因为三家的支付 ID
// 各有各的命名空间，裸 ID 有撞上的理论可能，而撞上的后果是**少发一次币**——
// 用户付了钱拿不到东西，且没有任何报错。
func (p OneTimePurchase) IdempotencyKey() string {
	return string(p.Provider) + ":" + p.ProviderPaymentID
}

// 这里原本有一张 CreditsByTier 表，webhook 收到 active / 续期时按档位赠送 credits。
// ADR-075 拆商品后整段删除：**基础订阅不含任何 credits**，credits 是独立购买的消耗品。
//
// 顺带消灭了一个真实缺陷：Stripe 的 customer.subscription.updated 在换卡、
// 改 metadata 这类无关变更时同样触发，而 credits_ledger 没有幂等键，
// 于是每触发一次就重发一整月额度。拆分后承载它的代码路径不存在了，无需修复。
