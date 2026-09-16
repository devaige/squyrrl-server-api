package subscriptions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Google Play 收单有两个入口，都以 Play Developer API 为准（见 play.go 的说明）：
//
//	POST /me/purchases/google-play   客户端买完立刻上报 purchaseToken
//	POST /webhooks/google            RTDN，经 Pub/Sub push 投递
//
// 这里此前是「RTDN 自带的字段就是真相」：状态取 notificationType，周期末尾用
// now+30/365 天**猜**出来，用户身份取 message.attributes["squyrrl_user_id"]。
// 最后一项尤其要记牢 —— Pub/Sub 从不设置那个属性，也没有任何一侧会去设置它，
// 所以那条 webhook 事实上解不出任何用户，每一条通知都以 ErrUnknownUser 结束。
// 现在三项全部回源去问 API。

// googlePushEnvelope Pub/Sub push 的外层结构。
// 请求身份由 Authorization 头上的 OIDC token 证明（GoogleOIDCVerifier，jws.go），
// 信封本身不承载任何可信信息。
type googlePushEnvelope struct {
	Message struct {
		Data string `json:"data"` // base64 的 RTDN JSON
	} `json:"message"`
}

// GoogleNotification 是一条 RTDN 里我们处理的三类通知。
//
// 刻意不带状态字段：notificationType 只当**触发信号**用。RTDN 会重投、会乱序，
// 按它写库意味着一条迟到的 RENEWED 能把一笔已经退款的订阅改回 active。
type GoogleNotification struct {
	PackageName string
	EventTime   time.Time

	// 三者至多一个非 nil。
	Subscription *GooglePurchaseRef
	OneTime      *GooglePurchaseRef
	Voided       *GoogleVoidedRef
}

// GooglePurchaseRef 一条通知指向的那笔购买。Type 是 RTDN 的 notificationType，
// 只用来分辨「这类通知要不要处理」，不用来决定状态。
type GooglePurchaseRef struct {
	PurchaseToken string
	ProductID     string
	Type          int
}

// GoogleVoidedRef 一笔被撤销的购买。ProductType：1 订阅 / 2 一次性。
type GoogleVoidedRef struct {
	PurchaseToken string
	ProductType   int
}

type googleRTDN struct {
	Version         string `json:"version"`
	PackageName     string `json:"packageName"`
	EventTimeMillis int64  `json:"eventTimeMillis,string"`

	SubscriptionNotification *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
		SubscriptionID   string `json:"subscriptionId"` // 'squyrrl_plan_basic_monthly'
	} `json:"subscriptionNotification"`
	OneTimeProductNotification *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
		SKU              string `json:"sku"`
	} `json:"oneTimeProductNotification"`
	VoidedPurchaseNotification *struct {
		PurchaseToken string `json:"purchaseToken"`
		OrderID       string `json:"orderId"`
		ProductType   int    `json:"productType"` // 1 SUBSCRIPTION / 2 ONE_TIME
	} `json:"voidedPurchaseNotification"`
	TestNotification *struct {
		Version string `json:"version"`
	} `json:"testNotification"`
}

// ParseGoogleRTDN 拆 Pub/Sub 信封并解出通知。**不做任何验签** ——
// 身份由 OIDC token 在 handler 里证明，这里拿到的仍是外部输入。
func ParseGoogleRTDN(payload []byte) (*GoogleNotification, error) {
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

	n := &GoogleNotification{
		PackageName: rtdn.PackageName,
		EventTime:   time.UnixMilli(rtdn.EventTimeMillis),
	}
	switch {
	case rtdn.SubscriptionNotification != nil:
		n.Subscription = &GooglePurchaseRef{
			PurchaseToken: rtdn.SubscriptionNotification.PurchaseToken,
			ProductID:     rtdn.SubscriptionNotification.SubscriptionID,
			Type:          rtdn.SubscriptionNotification.NotificationType,
		}
	case rtdn.OneTimeProductNotification != nil:
		n.OneTime = &GooglePurchaseRef{
			PurchaseToken: rtdn.OneTimeProductNotification.PurchaseToken,
			ProductID:     rtdn.OneTimeProductNotification.SKU,
			Type:          rtdn.OneTimeProductNotification.NotificationType,
		}
	case rtdn.VoidedPurchaseNotification != nil:
		n.Voided = &GoogleVoidedRef{
			PurchaseToken: rtdn.VoidedPurchaseNotification.PurchaseToken,
			ProductType:   rtdn.VoidedPurchaseNotification.ProductType,
		}
	default:
		// testNotification 和我们不关心的类型走同一条路：handler 回 200。
		// 对 4xx，Pub/Sub 会重投满 7 天再进死信 —— 「不关心」不是一种失败。
		return nil, ErrUnknownEvent
	}
	return n, nil
}

// GoogleEvent 与 AppleEvent 同形：订阅与一次性支付恰有一个非 nil。
type GoogleEvent struct {
	Subscription *SubscriptionEvent
	Purchase     *OneTimePurchase
}

// playSubStatus Play 的订阅状态 → 我们的四态。
//
// 关键的一条是 CANCELED **不映射成 canceled**：Play 的 CANCELED 只表示
// 「已关自动续期」，订阅到 expiryTime 之前照常有效，而用户为这段时间付过钱了。
// 映射成 canceled 会让 quota 那侧（只认 active / 宽限期内的 past_due）当场掐断
// 权益 —— 用户点一下「取消订阅」就立刻失去这个月剩下的天数。
// 真正结束由 EXPIRED 通知，或下一次回源查到的 expiryTime 已过。
var playSubStatus = map[string]string{
	"SUBSCRIPTION_STATE_ACTIVE":                    "active",
	"SUBSCRIPTION_STATE_CANCELED":                  "active",
	"SUBSCRIPTION_STATE_IN_GRACE_PERIOD":           "past_due",
	"SUBSCRIPTION_STATE_ON_HOLD":                   "past_due",
	"SUBSCRIPTION_STATE_PAUSED":                    "past_due",
	"SUBSCRIPTION_STATE_EXPIRED":                   "expired",
	"SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED": "expired",
}

// GoogleRedeem 查一笔 Play 购买并归一化成内部事件。客户端上报与 RTDN 共用这条路。
//
// productID 决定问哪个接口，但**它本身不被信任**：订阅的商品号以 API 返回的
// lineItems 为准；一次性商品的商品号是 API 路径的一部分，对不上就是 404。
// 也就是说客户端能影响的只有「去查哪一条记录」，查到什么与它无关。
func GoogleRedeem(
	ctx context.Context,
	api *PlayAPI,
	guard PlayGuard,
	productID, purchaseToken string,
	now time.Time,
) (*GoogleEvent, error) {
	if !api.Ready() {
		return nil, ErrPlayUnconfigured
	}
	sku, err := ParseProductID(productID, "_")
	if err != nil {
		return nil, err
	}
	if sku.Kind == "credits" {
		return googleOneTime(ctx, api, guard, productID, purchaseToken, sku)
	}
	return googleSubscription(ctx, api, guard, purchaseToken, now)
}

func googleSubscription(
	ctx context.Context,
	api *PlayAPI,
	guard PlayGuard,
	purchaseToken string,
	now time.Time,
) (*GoogleEvent, error) {
	sub, err := api.Subscription(ctx, purchaseToken)
	if err != nil {
		return nil, err
	}
	if sub.TestPurchase != nil && !guard.AllowTest {
		return nil, ErrWrongEnvironment
	}
	uid, err := googleUserID(sub.ExternalAccountIdentifiers.Obfuscated())
	if err != nil {
		return nil, err
	}
	if len(sub.LineItems) == 0 {
		return nil, ErrMalformedBody
	}
	// 多 line item 只在「一份订阅里含多个基础方案」时出现，我们没有这种商品；
	// 真出现了取第一条也比整笔丢弃强。
	item := sub.LineItems[0]
	sku, err := ParseProductID(item.ProductID, "_")
	if err != nil {
		return nil, err
	}

	status, ok := playSubStatus[sub.SubscriptionState]
	if !ok {
		// SUBSCRIPTION_STATE_PENDING（待付款，如巴西的 boleto）落到这里：
		// 钱还没到账就不落库。等付成了会再来一条 RTDN。
		return nil, ErrUnknownEvent
	}
	end, _ := time.Parse(time.RFC3339, item.ExpiryTime)
	if status == "active" && !end.IsZero() && end.Before(now) {
		status = "expired"
	}
	start, _ := time.Parse(time.RFC3339, sub.StartTime)

	evt := &SubscriptionEvent{
		Provider: ProviderGoogle,
		// purchaseToken 在整条续期链上不变，只有升降级 / 重新订阅会换新的 ——
		// 与 Apple 的 originalTransactionId 是同一个角色。
		ProviderSubscriptionID: purchaseToken,
		UserID:                 uid,
		Kind:                   sku.Kind,
		Tier:                   sku.Tier,
		BillingPeriod:          sku.Period,
		BonusStorageGB:         sku.StorageGB,
		Status:                 status,
		// startTime 是**首次开通**的时间而非本期开始 —— Play 没给后者。
		// 没有为此编一个「expiry 减去一个周期」的推算值：quota 那侧的宽限期只
		// 读 current_period_end，编出来的 start 不会有人用，却会有人信。
		PeriodStart: start,
		PeriodEnd:   end,
		// 升降档换 token 时，被换掉的那条只能从这里得知。
		SupersedesProviderID: sub.LinkedPurchaseToken,
	}
	if sub.SubscriptionState == "SUBSCRIPTION_STATE_CANCELED" {
		// 这是我们**观察到**取消的时间，不是用户点取消的时间 ——
		// Play 的订阅接口不提供后者。
		at := now
		evt.CanceledAt = &at
	}
	return &GoogleEvent{Subscription: evt}, nil
}

func googleOneTime(
	ctx context.Context,
	api *PlayAPI,
	guard PlayGuard,
	productID, purchaseToken string,
	sku SKU,
) (*GoogleEvent, error) {
	p, err := api.Product(ctx, productID, purchaseToken)
	if err != nil {
		return nil, err
	}
	// purchaseType 缺席才是正常购买：0 是「许可测试单」，不扣款。
	if p.PurchaseType != nil && !guard.AllowTest {
		return nil, ErrWrongEnvironment
	}
	if p.PurchaseState != 0 {
		// 1 已取消 / 2 待处理。待处理的会在付成后再来一条 RTDN。
		return nil, ErrUnknownEvent
	}
	uid, err := googleUserID(p.ObfuscatedExternalAccountID)
	if err != nil {
		return nil, err
	}
	qty := int64(p.Quantity)
	if qty <= 0 {
		qty = 1
	}
	id := p.OrderID
	if id == "" {
		// 理论上 orderId 恒有；真缺了就退回 token。消耗型每买一次是一个新 token，
		// 所以它同样能当幂等键，只是长得多。
		id = purchaseToken
	}
	return &GoogleEvent{Purchase: &OneTimePurchase{
		Provider:          ProviderGoogle,
		ProviderPaymentID: id,
		UserID:            uid,
		Credits:           sku.Credits * qty,
	}}, nil
}

// GoogleEventFromNotification 把一条 RTDN 变成内部事件，状态一律回源。
func GoogleEventFromNotification(
	ctx context.Context,
	api *PlayAPI,
	guard PlayGuard,
	n *GoogleNotification,
	now time.Time,
) (*GoogleEvent, error) {
	if !api.Ready() {
		return nil, ErrPlayUnconfigured
	}
	// 包名对不上说明这条通知不是发给我们这个应用的。API 路径里带的是我们自己的
	// 包名，所以放过去也只会 404，但那要多花一次出网才知道。
	if guard.PackageName != "" && n.PackageName != "" && n.PackageName != guard.PackageName {
		return nil, ErrWrongApp
	}

	switch {
	case n.Subscription != nil:
		return googleSubscription(ctx, api, guard, n.Subscription.PurchaseToken, now)

	case n.OneTime != nil:
		// 1 = ONE_TIME_PRODUCT_PURCHASED，2 = ONE_TIME_PRODUCT_CANCELED。
		// 只认 1：取消的单本来就没发过币。
		if n.OneTime.Type != 1 {
			return nil, ErrUnknownEvent
		}
		sku, err := ParseProductID(n.OneTime.ProductID, "_")
		if err != nil {
			return nil, err
		}
		if sku.Kind != "credits" {
			return nil, ErrUnknownEvent
		}
		return googleOneTime(ctx, api, guard, n.OneTime.ProductID, n.OneTime.PurchaseToken, sku)

	case n.Voided != nil:
		// 退款。订阅回源去查（会读到 EXPIRED / CANCELED）并照常落库。
		//
		// 一次性商品（代币）**不扣回**，与 Apple 侧的撤销同一个判断：币可能已经
		// 花掉了，强行扣成负数会让账户卡在一个用不了的状态。这类要人工处理。
		if n.Voided.ProductType != 1 {
			return nil, ErrUnknownEvent
		}
		return googleSubscription(ctx, api, guard, n.Voided.PurchaseToken, now)
	}
	return nil, ErrUnknownEvent
}

// googleUserID 从 obfuscatedExternalAccountId 解出账号。
//
// 与 Apple 的 appAccountToken 同一个角色、同一条铁律：解不出就**拒绝**，
// 不存在「退回到用别的办法猜是谁」。认不出归属的凭证只有两种结局 ——
// 发给错的人，或者不发；后者可以人工补，前者不能撤。
func googleUserID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrUnknownUser
	}
	return id, nil
}
