package subscriptions

import (
	"errors"
	"testing"
)

func session(mode, status, credits, user string) []byte {
	return []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{
		"id":"cs_test_123","mode":"` + mode + `","payment_status":"` + status + `",
		"metadata":{"squyrrl_user_id":"` + user + `","squyrrl_credits":"` + credits + `"}}}}`)
}

const goodUser = "11111111-1111-1111-1111-111111111111"

func TestParseStripeOneTime(t *testing.T) {
	p, err := ParseStripeOneTime(session("payment", "paid", "55000", goodUser))
	if err != nil {
		t.Fatalf("正常一次性支付应解析成功：%v", err)
	}
	if p.Credits != 55000 {
		t.Errorf("代币数应为 55000，实际 %d", p.Credits)
	}
	// 幂等键带 provider 前缀：三家的支付 ID 各有命名空间，裸 ID 撞上的后果是
	// **少发一次币** —— 用户付了钱拿不到东西，且没有任何报错。
	if got := p.IdempotencyKey(); got != "stripe:cs_test_123" {
		t.Errorf("幂等键应为 stripe:cs_test_123，实际 %s", got)
	}
}

// checkout.session.completed **对订阅结账同样触发**。不判 mode 的话，
// 每一笔订阅都会顺带发一次币 —— 正是 ADR-075 删掉的那个老缺陷的翻版。
func TestSubscriptionCheckoutDoesNotGrantCredits(t *testing.T) {
	if _, err := ParseStripeOneTime(session("subscription", "paid", "55000", goodUser)); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("订阅模式的 session 不该被当成代币购买，得到 %v", err)
	}
}

// Checkout 支持「先下单后付款」，那种 session 也会 completed，但钱还没到。
func TestUnpaidSessionIsIgnored(t *testing.T) {
	if _, err := ParseStripeOneTime(session("payment", "unpaid", "55000", goodUser)); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("未付款的 session 不该发币，得到 %v", err)
	}
}

// 认不出买了多少就不发：发多了收不回来，发少了用户会来说，而什么都没发两种都能补。
func TestMissingOrBadCreditsRejected(t *testing.T) {
	for _, v := range []string{"", "0", "-100", "abc"} {
		if _, err := ParseStripeOneTime(session("payment", "paid", v, goodUser)); err == nil {
			t.Errorf("credits=%q 应被拒绝", v)
		}
	}
}

func TestBadUserRejected(t *testing.T) {
	if _, err := ParseStripeOneTime(session("payment", "paid", "10000", "not-a-uuid")); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("非法 user_id 应判 ErrUnknownUser，得到 %v", err)
	}
}

// 非 checkout 事件一律不认，交回给订阅 parser / 忽略。
func TestOtherEventTypesIgnored(t *testing.T) {
	body := []byte(`{"id":"evt_2","type":"payment_intent.succeeded","data":{"object":{}}}`)
	if _, err := ParseStripeOneTime(body); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("payment_intent.succeeded 不该被处理（同一笔支付会经两个事件到达，"+
			"两条都处理就是两次发币的机会），得到 %v", err)
	}
}
