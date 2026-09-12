package subscriptions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// VerifyStripeSignature 校验 Stripe-Signature 头部
// 头格式：`t=<unix>,v1=<hmac>,v1=<hmac>` —— 任一 v1 匹配即视为有效
// payload 是 raw request body（注意不要 JSON 重新编码，必须字节级一致）
func VerifyStripeSignature(payload []byte, sigHeader, secret string, tolerance time.Duration) error {
	if secret == "" {
		return errors.New("stripe webhook secret not configured")
	}
	var ts string
	var v1s []string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			v1s = append(v1s, kv[1])
		}
	}
	if ts == "" || len(v1s) == 0 {
		return ErrBadSignature
	}
	tsNum, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if tolerance > 0 {
		diff := time.Since(time.Unix(tsNum, 0))
		if diff > tolerance || diff < -tolerance {
			return fmt.Errorf("stripe signature timestamp outside tolerance: %v", diff)
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, v := range v1s {
		if hmac.Equal([]byte(v), []byte(want)) {
			return nil
		}
	}
	return ErrBadSignature
}

// stripeEvent 只解析我们关心的 envelope 字段，业务相关字段下沉到 raw map
type stripeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type stripeSubObject struct {
	ID                 string          `json:"id"`
	Status             string          `json:"status"`
	CurrentPeriodStart int64           `json:"current_period_start"`
	CurrentPeriodEnd   int64           `json:"current_period_end"`
	CanceledAt         *int64          `json:"canceled_at"`
	Metadata           squyrrlMetadata `json:"metadata"`
}

// squyrrlMetadata 是我们在 Checkout Session / Price 上约定塞入的字段。
//
// 走 metadata 而不是回查一张 price_id → 商品 的映射表：webhook 必须在几百毫秒内
// 回 200，多一次数据库往返只是其次，真正的问题是那张表一旦与 Stripe 后台不同步，
// 表现就是**收了钱但认不出买的是什么**。让权威信息随事件一起到达。
type squyrrlMetadata struct {
	UserID string `json:"squyrrl_user_id"`
	// Kind 缺省为 plan：基础订阅是先上线的商品，存量的 Checkout Session
	// 上没有这个字段。新建的存储订阅必须显式带 squyrrl_kind=storage。
	Kind   string `json:"squyrrl_kind"`
	Tier   string `json:"squyrrl_tier"`
	Period string `json:"squyrrl_period"` // monthly / yearly
	// StorageGB 仅 kind=storage 时有意义，字符串是因为 Stripe metadata 的值恒为字符串。
	StorageGB string `json:"squyrrl_storage_gb"`
	// Credits 仅一次性支付时有意义。
	Credits string `json:"squyrrl_credits"`
}

// ParseStripeEvent 把 Stripe webhook 映射到内部事件结构。
// 只处理 customer.subscription.{created,updated,deleted}；其余返 ErrUnknownEvent 由 caller 200 OK 忽略。
//
// 业务侧约定：Stripe checkout session 创建时塞入 metadata.squyrrl_user_id / tier / period，
// 这样 webhook 不用回查 mapping 表就能定位用户。
func ParseStripeEvent(payload []byte) (*SubscriptionEvent, error) {
	var env stripeEvent
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, ErrMalformedBody
	}
	if !strings.HasPrefix(env.Type, "customer.subscription.") {
		return nil, ErrUnknownEvent
	}

	var wrap struct {
		Object stripeSubObject `json:"object"`
	}
	if err := json.Unmarshal(env.Data, &wrap); err != nil {
		return nil, ErrMalformedBody
	}
	obj := wrap.Object

	uid, err := uuid.Parse(obj.Metadata.UserID)
	if err != nil {
		return nil, ErrUnknownUser
	}

	status := obj.Status
	switch status {
	case "active", "trialing":
		status = "active"
	case "past_due":
		status = "past_due"
	case "canceled", "incomplete_expired":
		status = "canceled"
	case "unpaid":
		status = "past_due"
	default:
		status = "expired"
	}

	period := obj.Metadata.Period
	if period == "" {
		period = "monthly"
	}

	kind := obj.Metadata.Kind
	if kind == "" {
		kind = "plan"
	}
	if kind != "plan" && kind != "storage" {
		return nil, ErrUnknownEvent
	}

	// 存储订阅必须带容量：没有它，一笔已付款的订阅会落成 bonus_storage_gb = NULL，
	// 用户付了钱而配额纹丝不动 —— 而且这条记录看起来完全正常，没人会去查。
	var storageGB *int
	if kind == "storage" {
		gb, err := strconv.Atoi(obj.Metadata.StorageGB)
		if err != nil || gb <= 0 {
			return nil, ErrMalformedBody
		}
		storageGB = &gb
	}

	var canceled *time.Time
	if obj.CanceledAt != nil {
		t := time.Unix(*obj.CanceledAt, 0)
		canceled = &t
	}

	return &SubscriptionEvent{
		Provider:               ProviderStripe,
		ProviderSubscriptionID: obj.ID,
		UserID:                 uid,
		Kind:                   kind,
		Tier:                   obj.Metadata.Tier,
		BonusStorageGB:         storageGB,
		BillingPeriod:          period,
		Status:                 status,
		PeriodStart:            time.Unix(obj.CurrentPeriodStart, 0),
		PeriodEnd:              time.Unix(obj.CurrentPeriodEnd, 0),
		CanceledAt:             canceled,
	}, nil
}

// stripeCheckoutSession 是一次性支付（代币加购）到达的形态。
type stripeCheckoutSession struct {
	ID string `json:"id"`
	// Mode 区分这次结账卖的是订阅还是一次性商品。
	// checkout.session.completed **对订阅结账同样会触发**，不判 mode 的话，
	// 每笔订阅都会顺带发一次币。
	Mode string `json:"mode"`
	// PaymentStatus 必须是 paid。Checkout 支持「先下单后付款」（invoice 模式），
	// 那种 session 也会 completed，但钱还没到。
	PaymentStatus string          `json:"payment_status"`
	Metadata      squyrrlMetadata `json:"metadata"`
}

// ParseStripeOneTime 解析一次性支付事件（代币加购）。
//
// 只认 checkout.session.completed：代币是在结账页一次性买断的，没有续期概念。
// 刻意**不**接 payment_intent.succeeded —— 同一笔支付会经由两个事件到达，
// 两条都处理就是两次发币的机会，而幂等键只能救「同一事件重复投递」这一种。
//
// 与被删掉的那段旧逻辑的区别值得写清楚：那时是 customer.subscription.updated
// 触发发币，而该事件在换卡、改 metadata 这类无关变更时同样触发，于是每次都重发
// 一整月额度。现在发币的唯一入口是一次真实的、用户主动完成的结账。
func ParseStripeOneTime(payload []byte) (*OneTimePurchase, error) {
	var env stripeEvent
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, ErrMalformedBody
	}
	if env.Type != "checkout.session.completed" {
		return nil, ErrUnknownEvent
	}

	var wrap struct {
		Object stripeCheckoutSession `json:"object"`
	}
	if err := json.Unmarshal(env.Data, &wrap); err != nil {
		return nil, ErrMalformedBody
	}
	obj := wrap.Object

	if obj.Mode != "payment" || obj.PaymentStatus != "paid" {
		return nil, ErrUnknownEvent
	}

	uid, err := uuid.Parse(obj.Metadata.UserID)
	if err != nil {
		return nil, ErrUnknownUser
	}
	credits, err := strconv.ParseInt(obj.Metadata.Credits, 10, 64)
	if err != nil || credits <= 0 {
		// 认不出买了多少就不发。宁可让一笔支付挂在那里等人工处理，
		// 也不要凭猜测发一个数字出去 —— 发多了收不回来，发少了用户会来说，
		// 而「什么都没发」两种情况都能补。
		return nil, ErrMalformedBody
	}

	return &OneTimePurchase{
		Provider:          ProviderStripe,
		ProviderPaymentID: obj.ID,
		UserID:            uid,
		Credits:           credits,
	}, nil
}
