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
	Provider              Provider
	ProviderSubscriptionID string  // Stripe sub_xxx / Apple originalTransactionId / Google purchaseToken
	UserID                uuid.UUID // 必填：从 metadata / external account 取
	Kind                  string    // 'plan' | 'storage'
	Tier                  string    // 'basic'|'standard'|'premium'|'maximum' 或 storage tier
	BillingPeriod         string    // 'monthly' | 'yearly'
	Status                string    // 'active' | 'past_due' | 'canceled' | 'expired'
	PeriodStart           time.Time
	PeriodEnd             time.Time
	CanceledAt            *time.Time
	BonusStorageGB        *int      // storage 类型时填入
}

// 这里原本有一张 CreditsByTier 表，webhook 收到 active / 续期时按档位赠送 credits。
// ADR-075 拆商品后整段删除：**基础订阅不含任何 credits**，credits 是独立购买的消耗品。
//
// 顺带消灭了一个真实缺陷：Stripe 的 customer.subscription.updated 在换卡、
// 改 metadata 这类无关变更时同样触发，而 credits_ledger 没有幂等键，
// 于是每触发一次就重发一整月额度。拆分后承载它的代码路径不存在了，无需修复。
