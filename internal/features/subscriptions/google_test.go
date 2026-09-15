package subscriptions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// rtdn 拼一条 Pub/Sub push 请求体：外层信封 + base64 的 RTDN。
func rtdn(t *testing.T, inner map[string]any) []byte {
	t.Helper()
	if _, ok := inner["packageName"]; !ok {
		inner["packageName"] = playPkg
	}
	if _, ok := inner["eventTimeMillis"]; !ok {
		inner["eventTimeMillis"] = "1757836800000"
	}
	raw, _ := json.Marshal(inner)
	env, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":      base64.StdEncoding.EncodeToString(raw),
			"messageId": "1",
		},
		"subscription": "projects/squyrrl/subscriptions/rtdn",
	})
	return env
}

func TestParseGoogleRTDNSubscription(t *testing.T) {
	n, err := ParseGoogleRTDN(rtdn(t, map[string]any{
		"subscriptionNotification": map[string]any{
			"version":          "1.0",
			"notificationType": 2,
			"purchaseToken":    playToken,
			"subscriptionId":   "squyrrl_plan_basic_monthly",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if n.Subscription == nil || n.Subscription.PurchaseToken != playToken {
		t.Fatalf("解错了: %+v", n)
	}
	if n.PackageName != playPkg {
		t.Errorf("packageName = %q", n.PackageName)
	}
}

func TestParseGoogleRTDNOneTimeAndVoided(t *testing.T) {
	n, err := ParseGoogleRTDN(rtdn(t, map[string]any{
		"oneTimeProductNotification": map[string]any{
			"notificationType": 1,
			"purchaseToken":    playToken,
			"sku":              "squyrrl_credits_p5_once",
		},
	}))
	if err != nil || n.OneTime == nil || n.OneTime.ProductID != "squyrrl_credits_p5_once" {
		t.Fatalf("一次性通知解错了: %+v, %v", n, err)
	}

	n, err = ParseGoogleRTDN(rtdn(t, map[string]any{
		"voidedPurchaseNotification": map[string]any{
			"purchaseToken": playToken,
			"orderId":       "GPA.1",
			"productType":   1,
		},
	}))
	if err != nil || n.Voided == nil || n.Voided.ProductType != 1 {
		t.Fatalf("退款通知解错了: %+v, %v", n, err)
	}
}

// 「发送测试通知」按钮和我们不关心的类型走同一条路：handler 回 200。
// 对 4xx，Pub/Sub 会重投满 7 天。
func TestParseGoogleRTDNTestNotificationIsIgnored(t *testing.T) {
	_, err := ParseGoogleRTDN(rtdn(t, map[string]any{
		"testNotification": map[string]any{"version": "1.0"},
	}))
	if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v，想要 ErrUnknownEvent", err)
	}
}

func TestParseGoogleRTDNMalformed(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"message":{"data":"!!!not base64!!!"}}`),
		[]byte(`{"message":{"data":"bm90IGpzb24="}}`), // base64("not json")
	} {
		if _, err := ParseGoogleRTDN(body); !errors.Is(err, ErrMalformedBody) {
			t.Errorf("%s → %v，想要 ErrMalformedBody", body, err)
		}
	}
}

// RTDN 自带的 notificationType 只当触发信号：状态一律回源。
// 一条迟到的 RENEWED 不该把一笔已经退款的订阅改回 active。
func TestGoogleNotificationStateComesFromAPINotPayload(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"subscriptionState": "SUBSCRIPTION_STATE_EXPIRED",
	})))
	n, err := ParseGoogleRTDN(rtdn(t, map[string]any{
		"subscriptionNotification": map[string]any{
			"notificationType": 2, // RENEWED
			"purchaseToken":    playToken,
			"subscriptionId":   "squyrrl_plan_basic_monthly",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	evt, err := GoogleEventFromNotification(context.Background(), api, playGuardProd, n, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "expired" {
		t.Errorf("status = %s，通知说 RENEWED 但 API 说 EXPIRED，应当信 API",
			evt.Subscription.Status)
	}
}

func TestGoogleNotificationWrongPackageRejected(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, nil)))
	n, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"packageName": "com.someone.else",
		"subscriptionNotification": map[string]any{
			"notificationType": 4,
			"purchaseToken":    playToken,
			"subscriptionId":   "squyrrl_plan_basic_monthly",
		},
	}))
	_, err := GoogleEventFromNotification(context.Background(), api, playGuardProd, n, time.Now())
	if !errors.Is(err, ErrWrongApp) {
		t.Errorf("err = %v，想要 ErrWrongApp", err)
	}
}

func TestGoogleNotificationOneTimePurchased(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(productJSON(t, nil)))
	n, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"oneTimeProductNotification": map[string]any{
			"notificationType": 1,
			"purchaseToken":    playToken,
			"sku":              "squyrrl_credits_p5_once",
		},
	}))
	evt, err := GoogleEventFromNotification(context.Background(), api, playGuardProd, n, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Purchase == nil || evt.Purchase.Credits != 55_000 {
		t.Fatalf("代币没发对: %+v", evt.Purchase)
	}

	// 2 = CANCELED，本来就没发过币，不处理。
	n2, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"oneTimeProductNotification": map[string]any{
			"notificationType": 2,
			"purchaseToken":    playToken,
			"sku":              "squyrrl_credits_p5_once",
		},
	}))
	if _, err := GoogleEventFromNotification(
		context.Background(), api, playGuardProd, n2, time.Now(),
	); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v，想要 ErrUnknownEvent", err)
	}
}

// 退款：订阅回源落库，代币**不扣回**（币可能已经花掉，扣成负数会把账户
// 卡在一个用不了的状态）。与 Apple 侧对撤销的消耗型同一个判断。
func TestGoogleNotificationVoided(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"subscriptionState": "SUBSCRIPTION_STATE_EXPIRED",
	})))
	n, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"voidedPurchaseNotification": map[string]any{
			"purchaseToken": playToken,
			"productType":   1,
		},
	}))
	evt, err := GoogleEventFromNotification(context.Background(), api, playGuardProd, n, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "expired" {
		t.Errorf("退款后 status = %s", evt.Subscription.Status)
	}

	n2, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"voidedPurchaseNotification": map[string]any{
			"purchaseToken": playToken,
			"productType":   2, // 一次性
		},
	}))
	if _, err := GoogleEventFromNotification(
		context.Background(), api, playGuardProd, n2, time.Now(),
	); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("代币退款应当不处理，得到 %v", err)
	}
}

// 未配置服务账号时 RTDN 必须让 Pub/Sub 重投，而不是当作「已处理」吞掉 ——
// 那条续期事件不会再来第二次。
func TestGoogleNotificationUnconfiguredIsRetryable(t *testing.T) {
	n, _ := ParseGoogleRTDN(rtdn(t, map[string]any{
		"subscriptionNotification": map[string]any{
			"notificationType": 2,
			"purchaseToken":    playToken,
			"subscriptionId":   "squyrrl_plan_basic_monthly",
		},
	}))
	var nilAPI *PlayAPI
	_, err := GoogleEventFromNotification(context.Background(), nilAPI, playGuardProd, n, time.Now())
	if !errors.Is(err, ErrPlayUnconfigured) {
		t.Errorf("err = %v，想要 ErrPlayUnconfigured", err)
	}
	if got := googlePurchaseStatus(err); got != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，想要 503（可重试）", got)
	}
}

func TestGooglePurchaseStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrStoreUnavailable, http.StatusServiceUnavailable},
		{ErrPlayUnconfigured, http.StatusServiceUnavailable},
		{ErrUnknownEvent, http.StatusAccepted},
		{ErrUnknownPurchase, http.StatusBadRequest},
		{ErrUnknownTier, http.StatusBadRequest},
		{ErrWrongEnvironment, http.StatusBadRequest},
		{ErrUnknownUser, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := googlePurchaseStatus(c.err); got != c.want {
			t.Errorf("%v → %d，想要 %d", c.err, got, c.want)
		}
	}
}
