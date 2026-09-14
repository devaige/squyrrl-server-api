package subscriptions

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// App Store 的一切都是签名过的 JWS：服务器通知（ASSN V2）外层是
// {"signedPayload": "<JWS>"}，里面 data.signedTransactionInfo 又是一层 JWS；
// 客户端 StoreKit 2 拿到的 Transaction 也能直接给出同一种 JWS
// （VerificationResult.jwsRepresentation）。所以 webhook 与客户端上报两条路
// 最终解出来的是**同一个结构**，只在「状态从哪来」上分叉。
//
// 这里此前是一段共享密钥 HMAC，要求苹果带一个 X-Squyrrl-Sig 头 —— 苹果从不发
// 这个头，也没有任何地方能配置它。也就是说那条 webhook 实际上从未被验证过。

// AppleTransaction 是 JWSTransactionDecodedPayload 里我们用得到的字段。
type AppleTransaction struct {
	TransactionID         string `json:"transactionId"`
	OriginalTransactionID string `json:"originalTransactionId"`
	BundleID              string `json:"bundleId"`
	ProductID             string `json:"productId"`
	PurchaseDate          int64  `json:"purchaseDate"` // ms epoch
	ExpiresDate           int64  `json:"expiresDate"`  // ms epoch，消耗型没有
	RevocationDate        int64  `json:"revocationDate"`
	Quantity              int    `json:"quantity"`
	Type                  string `json:"type"` // Auto-Renewable Subscription / Consumable / …
	// AppAccountToken 由客户端在发起购买时填入，是凭证与 Squyrrl 账号之间
	// **唯一**的关联。客户端必须把它设成当前用户的 UUID。
	AppAccountToken    string `json:"appAccountToken"`
	InAppOwnershipType string `json:"inAppOwnershipType"`
	Environment        string `json:"environment"` // Production / Sandbox
}

// AppleEvent 是一次苹果交易归一化后的结果，两者恰有一个非 nil。
// 订阅与一次性支付的生命周期完全不同（见 OneTimePurchase 的说明），
// 合并成一个结构只会让 service 里长出一堆互斥分支。
type AppleEvent struct {
	Subscription *SubscriptionEvent
	Purchase     *OneTimePurchase
}

// AppleGuard 是收单前的三道闸门，全部来自部署配置。
//
// **少了它们，验签通过本身毫无意义**：苹果的根证书能验证的不只是我们这个 App
// 的凭证，而是 App Store 上任何一个 App 的凭证。没有 bundleId 比对，任何人在
// 别的应用里买一件 $0.99 的东西，把那张凭证发到这里就能换走一年的存储；
// Sandbox 的凭证同样由苹果正常签发，但**背后没有任何真实付款**。
// 所以两项缺一不可，缺了就整体不收单（fail-closed），而不是放行。
type AppleGuard struct {
	BundleID    string
	Environment string // Production | Sandbox
}

func (g AppleGuard) Ready() bool { return g.BundleID != "" && g.Environment != "" }

func (g AppleGuard) Check(t *AppleTransaction) error {
	if !g.Ready() {
		return ErrAppleUnconfigured
	}
	if t.BundleID != g.BundleID {
		return ErrWrongApp
	}
	if t.Environment != g.Environment {
		return ErrWrongEnvironment
	}
	return nil
}

// UserID 从 appAccountToken 解出账号。空值是**拒绝**而不是「回退到别的办法」：
// 认不出归属的凭证只有两种结局 —— 发给错的人，或者不发。后者可以人工补，
// 前者不能撤。
func (t *AppleTransaction) UserID() (uuid.UUID, error) {
	id, err := uuid.Parse(t.AppAccountToken)
	if err != nil {
		return uuid.Nil, ErrUnknownUser
	}
	return id, nil
}

// VerifyAppleTransaction 验签并解出一笔交易。
func VerifyAppleTransaction(jws string, now time.Time) (*AppleTransaction, error) {
	payload, err := VerifyAppleJWS(jws, now)
	if err != nil {
		return nil, err
	}
	var t AppleTransaction
	if err := json.Unmarshal(payload, &t); err != nil {
		return nil, ErrMalformedBody
	}
	if t.TransactionID == "" || t.ProductID == "" {
		return nil, ErrMalformedBody
	}
	if t.OriginalTransactionID == "" {
		t.OriginalTransactionID = t.TransactionID
	}
	return &t, nil
}

// appleNotificationPayload ASSN V2 解签后的信封（只解用得到的字段）。
type appleNotificationPayload struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	Data             struct {
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	} `json:"data"`
}

// appleStatusByNotification 服务器通知的类型 → 订阅状态。
var appleStatusByNotification = map[string]string{
	"SUBSCRIBED":                "active",
	"DID_RENEW":                 "active",
	"OFFER_REDEEMED":            "active",
	"DID_CHANGE_RENEWAL_PREF":   "active",
	"DID_CHANGE_RENEWAL_STATUS": "active",
	"DID_FAIL_TO_RENEW":         "past_due",
	"GRACE_PERIOD_EXPIRED":      "past_due",
	"EXPIRED":                   "expired",
	"REVOKE":                    "expired",
	"REFUND":                    "expired",
}

// ParseAppleNotification 校验并解析一条 ASSN V2 通知。
//
// 不认识的通知类型返回 ErrUnknownEvent，由 handler 回 200 —— 苹果对 4xx 会持续
// 重投并最终把端点判为不可达，而「我们不关心这个类型」不是一种失败。
func ParseAppleNotification(body []byte, guard AppleGuard, now time.Time) (*AppleEvent, error) {
	var env struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.SignedPayload == "" {
		return nil, ErrMalformedBody
	}
	payload, err := VerifyAppleJWS(env.SignedPayload, now)
	if err != nil {
		return nil, err
	}
	var n appleNotificationPayload
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, ErrMalformedBody
	}
	if n.NotificationType == "TEST" || n.Data.SignedTransactionInfo == "" {
		return nil, ErrUnknownEvent
	}
	txn, err := VerifyAppleTransaction(n.Data.SignedTransactionInfo, now)
	if err != nil {
		return nil, err
	}
	// webhook 与客户端上报过的是同一道闸门。苹果按 bundle 推送，理论上不会串台，
	// 但「理论上不会」和「串了会怎样」是两件事 —— 后者是给别的 App 的买家发
	// 我们的权益。
	if err := guard.Check(txn); err != nil {
		return nil, err
	}
	status, ok := appleStatusByNotification[n.NotificationType]
	if !ok {
		return nil, ErrUnknownEvent
	}
	return AppleEventFrom(txn, status, now)
}

// AppleEventFrom 把一笔已验签的交易映射成内部事件。
//
// status 为空表示「按交易自身推断」—— 客户端上报走这条路：它手上没有通知类型，
// 只有一张凭证，能说明的只有「这笔买成了，且截至 expiresDate 有效」。
func AppleEventFrom(t *AppleTransaction, status string, now time.Time) (*AppleEvent, error) {
	uid, err := t.UserID()
	if err != nil {
		return nil, err
	}
	sku, err := ParseProductID(t.ProductID, ".")
	if err != nil {
		return nil, err
	}

	if sku.Kind == "credits" {
		// 退款/撤销的消耗型不发币。**也不扣回** —— 币可能已经花掉了，
		// 强行扣成负数会让账户卡在一个用不了的状态；这类要人工处理。
		if t.RevocationDate > 0 {
			return nil, ErrUnknownEvent
		}
		qty := int64(t.Quantity)
		if qty <= 0 {
			qty = 1
		}
		return &AppleEvent{Purchase: &OneTimePurchase{
			Provider: ProviderApple,
			// 幂等键用 transactionId 而不是 originalTransactionId：消耗型可以
			// 反复购买，同一个 original 下会有很多笔，用 original 去重等于
			// 「第二次买不发币」。
			ProviderPaymentID: t.TransactionID,
			UserID:            uid,
			Credits:           sku.Credits * qty,
		}}, nil
	}

	if status == "" {
		status = "active"
		if t.ExpiresDate > 0 && time.UnixMilli(t.ExpiresDate).Before(now) {
			status = "expired"
		}
	}
	if t.RevocationDate > 0 {
		status = "expired"
	}

	evt := &SubscriptionEvent{
		Provider: ProviderApple,
		// 订阅用 originalTransactionId：它在整条续期链上保持不变，正是
		// Upsert 需要的那个「同一份订阅」的键。
		ProviderSubscriptionID: t.OriginalTransactionID,
		UserID:                 uid,
		Kind:                   sku.Kind,
		Tier:                   sku.Tier,
		BillingPeriod:          sku.Period,
		BonusStorageGB:         sku.StorageGB,
		Status:                 status,
		PeriodStart:            time.UnixMilli(t.PurchaseDate),
		PeriodEnd:              time.UnixMilli(t.ExpiresDate),
	}
	if t.RevocationDate > 0 {
		at := time.UnixMilli(t.RevocationDate)
		evt.CanceledAt = &at
	}
	return &AppleEvent{Subscription: evt}, nil
}
