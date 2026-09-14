package subscriptions

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Google Play RTDN 用 Pub/Sub push 方式投递，body 是
// {"message": {"data": "<base64 of JSON>", ...}}
// 内层 JSON 即 SubscriptionNotification 结构。
//
// 请求身份由 Pub/Sub 在 Authorization 头上的 OIDC token 证明，验签见
// GoogleOIDCVerifier（jws.go）。此前那段只解 payload 比对 aud、**不验签名**
// 的实现已删除：/webhooks/google 挂在公网根上、没有 Bearer 中间件，
// 伪造一个 aud 对得上的 base64 串就能给任意用户开任意档位的订阅。

// googlePushEnvelope Pub/Sub push 的外层结构
type googlePushEnvelope struct {
	Message struct {
		Data       string            `json:"data"` // base64-encoded
		Attributes map[string]string `json:"attributes"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

type googleRTDN struct {
	Version                  string `json:"version"`
	PackageName              string `json:"packageName"`
	EventTimeMillis          int64  `json:"eventTimeMillis,string"`
	SubscriptionNotification *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
		SubscriptionID   string `json:"subscriptionId"` // 'squyrrl_basic_monthly'
	} `json:"subscriptionNotification"`
}

// Google RTDN notificationType:
//
//	 1 RECOVERED  2 RENEWED  3 CANCELED  4 PURCHASED
//	 5 ON_HOLD    6 IN_GRACE 7 RESTARTED 8 PRICE_CHANGE
//	10 PAUSED    12 REVOKED 13 EXPIRED
//
// 我们只关心 status 映射：
var googleStatus = map[int]string{
	1: "active", 2: "active", 4: "active", 7: "active",
	3: "canceled", 12: "canceled",
	5: "past_due", 6: "past_due", 10: "past_due", 8: "active",
	13: "expired",
}

// ParseGoogleRTDN 解析 Pub/Sub envelope + 内嵌 RTDN，
// 业务侧约定 Play Console 配置 obfuscatedAccountId = squyrrl uuid（解析后从 attributes 取或注入）。
// Phase 1.5 简化：从 message.attributes["squyrrl_user_id"] 取，由发送方网关注入。
// 完整版需调用 Google Subscriptions:get API 取 obfuscatedExternalAccountId。
func ParseGoogleRTDN(payload []byte) (*SubscriptionEvent, error) {
	var env googlePushEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, ErrMalformedBody
	}
	inner, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil {
		return nil, ErrMalformedBody
	}
	var rtdn googleRTDN
	if err := json.Unmarshal(inner, &rtdn); err != nil {
		return nil, ErrMalformedBody
	}
	if rtdn.SubscriptionNotification == nil {
		return nil, ErrUnknownEvent
	}

	userStr := env.Message.Attributes["squyrrl_user_id"]
	uid, err := uuid.Parse(userStr)
	if err != nil {
		return nil, ErrUnknownUser
	}

	// subscriptionId 命名约定见 ParseProductID：'squyrrl_<kind>_<tier>_<period>'，
	// 三段式（无 kind）向后兼容为 plan。
	sku, err := ParseProductID(rtdn.SubscriptionNotification.SubscriptionID, "_")
	if err != nil {
		return nil, err
	}

	status, ok := googleStatus[rtdn.SubscriptionNotification.NotificationType]
	if !ok {
		status = "expired"
	}

	now := time.UnixMilli(rtdn.EventTimeMillis)
	// Google webhook 不直接给 period_end —— 简化用 +30 天 / +365 天
	var end time.Time
	switch sku.Period {
	case "yearly":
		end = now.AddDate(1, 0, 0)
	default:
		end = now.AddDate(0, 1, 0)
	}

	return &SubscriptionEvent{
		Provider:               ProviderGoogle,
		ProviderSubscriptionID: rtdn.SubscriptionNotification.PurchaseToken,
		UserID:                 uid,
		Kind:                   sku.Kind,
		Tier:                   sku.Tier,
		BonusStorageGB:         sku.StorageGB,
		BillingPeriod:          sku.Period,
		Status:                 status,
		PeriodStart:            now,
		PeriodEnd:              end,
	}, nil
}
