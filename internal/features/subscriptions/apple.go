package subscriptions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// VerifyAppleSignature 简化版校验：
// App Store Server Notifications V2 实际用 JWS（Apple 根证书 + ECDSA）。
// 完整实现需引入 ASN.1/X.509 + apple root anchor，Phase 1.5 先用 shared-secret HMAC 兜底。
//
// 我们要求 Apple 推送时配一个 `X-Squyrrl-Sig: <hex(hmac-sha256(body, secret))>` 头
// （通过一个中转 Cloudflare Worker / Apple-side proxy 配置），完整 JWS 校验在 Phase 2 接入。
func VerifyAppleSignature(payload []byte, sig, secret string) error {
	if secret == "" {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}

// appleNotification ASSN V2 envelope（仅解我们需要的字段）
type appleNotification struct {
	NotificationType string `json:"notificationType"` // SUBSCRIBED / DID_RENEW / DID_FAIL_TO_RENEW / EXPIRED / REVOKE
	Subtype          string `json:"subtype"`
	Data             struct {
		// 业务约定：在 App Store Connect 配置 App Account Token =/= UUID 形式的 user_id
		// 这样 webhook 不用回查 receipt verification 即可定位用户
		AppAccountToken        string `json:"appAccountToken"`
		ProductID              string `json:"productId"`           // 'squyrrl.basic.monthly' …
		OriginalTransactionID  string `json:"originalTransactionId"`
		ExpiresDate            int64  `json:"expiresDate"`         // ms epoch
		PurchaseDate           int64  `json:"purchaseDate"`        // ms epoch
	} `json:"data"`
}

// ParseAppleNotification 把 ASSN V2 映射成 SubscriptionEvent。
// productId 命名约定：`squyrrl.<tier>.<period>` 例如 `squyrrl.basic.monthly`。
func ParseAppleNotification(payload []byte) (*SubscriptionEvent, error) {
	var n appleNotification
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, ErrMalformedBody
	}
	if n.Data.OriginalTransactionID == "" {
		return nil, ErrMalformedBody
	}

	uid, err := uuid.Parse(n.Data.AppAccountToken)
	if err != nil {
		return nil, ErrUnknownUser
	}

	parts := strings.Split(n.Data.ProductID, ".")
	if len(parts) != 3 {
		return nil, ErrUnknownTier
	}
	tier, period := parts[1], parts[2]

	status := "active"
	switch n.NotificationType {
	case "DID_FAIL_TO_RENEW":
		status = "past_due"
	case "EXPIRED", "REVOKE":
		status = "expired"
	case "SUBSCRIBED", "DID_RENEW":
		status = "active"
	}

	return &SubscriptionEvent{
		Provider:               ProviderApple,
		ProviderSubscriptionID: n.Data.OriginalTransactionID,
		UserID:                 uid,
		Kind:                   "plan",
		Tier:                   tier,
		BillingPeriod:          period,
		Status:                 status,
		PeriodStart:            time.UnixMilli(n.Data.PurchaseDate),
		PeriodEnd:              time.UnixMilli(n.Data.ExpiresDate),
	}, nil
}

// 让 base64 包不被 dead-code lint 嫌弃（Phase 2 JWS 校验时会用到）
var _ = base64.StdEncoding
