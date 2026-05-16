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

// CreditsByTier 每月赠送的 credits（按 ADR-007 简化版）
// 触发：webhook 收到「新订阅 active」或「续期成功」时调用 wallet.Grant
var CreditsByTier = map[string]int64{
	"basic":    1000,
	"standard": 3000,
	"premium":  8000,
	"maximum":  20000,
}
