package subscriptions

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Google Play RTDN 用 Pub/Sub push 方式投递，body 是
// {"message": {"data": "<base64 of JSON>", ...}}
// 内层 JSON 即 SubscriptionNotification 结构。
//
// 验签由 Cloud Pub/Sub 用 OIDC bearer token 在 Authorization 头上完成；
// 我们检查这个 token 的 audience 是否匹配 SQUYRRL_GOOGLE_PUBSUB_AUD。
// 完整 RSA 验签留作 Phase 2，目前先做 audience claim 校验。

// VerifyGoogleOIDC 简化校验：解析 JWT payload（不验签）拿 aud 字段比对。
// 注意：仅当部署在 GCP 内网 / 经由可信网关时安全；公网部署需补 Google JWKS RSA 校验。
func VerifyGoogleOIDC(bearer, expectedAud string) error {
	if expectedAud == "" {
		return ErrBadSignature
	}
	parts := strings.Split(strings.TrimPrefix(bearer, "Bearer "), ".")
	if len(parts) != 3 {
		return ErrBadSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrBadSignature
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ErrBadSignature
	}
	if claims.Aud != expectedAud {
		return ErrBadSignature
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return ErrBadSignature
	}
	return nil
}

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

	// subscriptionId 命名约定：'squyrrl_<tier>_<period>'
	parts := strings.Split(rtdn.SubscriptionNotification.SubscriptionID, "_")
	if len(parts) < 3 {
		return nil, ErrUnknownTier
	}
	tier, period := parts[1], parts[2]

	status, ok := googleStatus[rtdn.SubscriptionNotification.NotificationType]
	if !ok {
		status = "expired"
	}

	now := time.UnixMilli(rtdn.EventTimeMillis)
	// Google webhook 不直接给 period_end —— 简化用 +30 天 / +365 天
	var end time.Time
	switch period {
	case "yearly":
		end = now.AddDate(1, 0, 0)
	default:
		end = now.AddDate(0, 1, 0)
	}

	return &SubscriptionEvent{
		Provider:               ProviderGoogle,
		ProviderSubscriptionID: rtdn.SubscriptionNotification.PurchaseToken,
		UserID:                 uid,
		Kind:                   "plan",
		Tier:                   tier,
		BillingPeriod:          period,
		Status:                 status,
		PeriodStart:            now,
		PeriodEnd:              end,
	}, nil
}
