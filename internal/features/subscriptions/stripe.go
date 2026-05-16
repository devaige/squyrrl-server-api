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
	ID                 string `json:"id"`
	Status             string `json:"status"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	CanceledAt         *int64 `json:"canceled_at"`
	Metadata           struct {
		UserID string `json:"squyrrl_user_id"`
		Tier   string `json:"squyrrl_tier"`
		Period string `json:"squyrrl_period"` // monthly / yearly
	} `json:"metadata"`
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

	var canceled *time.Time
	if obj.CanceledAt != nil {
		t := time.Unix(*obj.CanceledAt, 0)
		canceled = &t
	}

	return &SubscriptionEvent{
		Provider:               ProviderStripe,
		ProviderSubscriptionID: obj.ID,
		UserID:                 uid,
		Kind:                   "plan",
		Tier:                   obj.Metadata.Tier,
		BillingPeriod:          period,
		Status:                 status,
		PeriodStart:            time.Unix(obj.CurrentPeriodStart, 0),
		PeriodEnd:              time.Unix(obj.CurrentPeriodEnd, 0),
		CanceledAt:             canceled,
	}, nil
}
