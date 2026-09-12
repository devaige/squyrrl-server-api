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
)

// Provider 区分三个支付渠道。落库到 subscriptions.payment_provider 字段。
type Provider string

const (
	ProviderStripe Provider = "stripe"
	ProviderApple  Provider = "app_store"
	ProviderGoogle Provider = "google_play"
)

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
