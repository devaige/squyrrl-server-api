package subscriptions

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const (
	testBundle = "com.squyrrl.app"
	testUser   = "22222222-2222-2222-2222-222222222222"
)

var prodGuard = AppleGuard{BundleID: testBundle, Environment: "Production"}

func txnPayload(overrides map[string]any) map[string]any {
	p := map[string]any{
		"transactionId":         "2000000100000001",
		"originalTransactionId": "2000000000000001",
		"bundleId":              testBundle,
		"productId":             "squyrrl.plan.basic.monthly",
		"purchaseDate":          time.Now().Add(-time.Hour).UnixMilli(),
		"expiresDate":           time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
		"quantity":              1,
		"type":                  "Auto-Renewable Subscription",
		"appAccountToken":       testUser,
		"inAppOwnershipType":    "PURCHASED",
		"environment":           "Production",
	}
	for k, v := range overrides {
		if v == nil {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	return p
}

// appleFixture 造一条可信链并返回「签一笔交易」的闭包。
func appleFixture(t *testing.T) func(map[string]any) string {
	t.Helper()
	leaf, x5c := fakeAppleChain(t)
	return func(over map[string]any) string {
		return signAppleJWS(t, leaf.key, x5c, "ES256", txnPayload(over))
	}
}

// 苹果的根证书能验证 App Store 上**任何一个 App** 的凭证。少了这两道闸门，
// 在别的应用里买一件 $0.99 的东西就能换走这里的一年存储；Sandbox 的凭证由苹果
// 正常签发，但背后没有任何真实付款。
func TestAppleGuard(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()

	other, err := VerifyAppleTransaction(sign(map[string]any{"bundleId": "com.someone.else"}), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := prodGuard.Check(other); !errors.Is(err, ErrWrongApp) {
		t.Errorf("别的 App 的凭证应判 ErrWrongApp，得到 %v", err)
	}

	sandbox, err := VerifyAppleTransaction(sign(map[string]any{"environment": "Sandbox"}), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := prodGuard.Check(sandbox); !errors.Is(err, ErrWrongEnvironment) {
		t.Errorf("Sandbox 凭证应判 ErrWrongEnvironment，得到 %v", err)
	}

	ok, err := VerifyAppleTransaction(sign(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := prodGuard.Check(ok); err != nil {
		t.Errorf("本应用的生产凭证应通过：%v", err)
	}
	// 漏配不是「放行」而是「整体不收单」。
	if err := (AppleGuard{}).Check(ok); !errors.Is(err, ErrAppleUnconfigured) {
		t.Errorf("未配置应判 ErrAppleUnconfigured，得到 %v", err)
	}
	if (AppleGuard{BundleID: testBundle}).Ready() {
		t.Error("只配了 bundleId 不该算就绪")
	}
}

func TestAppleEventSubscription(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()
	txn, err := VerifyAppleTransaction(sign(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	evt, err := AppleEventFrom(txn, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription == nil || evt.Purchase != nil {
		t.Fatal("订阅商品应落成 SubscriptionEvent")
	}
	s := evt.Subscription
	// 续期链上 transactionId 每期都变，originalTransactionId 不变 ——
	// 用前者做键会让每次续费都插一条新订阅。
	if s.ProviderSubscriptionID != "2000000000000001" {
		t.Errorf("订阅键应是 originalTransactionId，实际 %s", s.ProviderSubscriptionID)
	}
	if s.Status != "active" || s.Tier != "basic" || s.BillingPeriod != "monthly" {
		t.Errorf("落库内容不对：%+v", s)
	}
	if s.UserID.String() != testUser {
		t.Errorf("用户应来自 appAccountToken，实际 %s", s.UserID)
	}
}

func TestAppleEventStorageCarriesQuota(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()
	txn, err := VerifyAppleTransaction(
		sign(map[string]any{"productId": "squyrrl.storage.s1000.yearly"}), now)
	if err != nil {
		t.Fatal(err)
	}
	evt, err := AppleEventFrom(txn, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.BonusStorageGB == nil || *evt.Subscription.BonusStorageGB != 1000 {
		t.Fatalf("容量应为 1000，实际 %v", evt.Subscription.BonusStorageGB)
	}
}

func TestAppleEventConsumableGrantsCredits(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()
	txn, err := VerifyAppleTransaction(sign(map[string]any{
		"productId":   "squyrrl.credits.p5.once",
		"type":        "Consumable",
		"quantity":    2,
		"expiresDate": nil,
	}), now)
	if err != nil {
		t.Fatal(err)
	}
	evt, err := AppleEventFrom(txn, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Purchase == nil || evt.Subscription != nil {
		t.Fatal("消耗型商品应落成 OneTimePurchase")
	}
	// 一次买两份就是两份的币。
	if evt.Purchase.Credits != 110000 {
		t.Errorf("代币数应为 110000，实际 %d", evt.Purchase.Credits)
	}
	// 幂等键必须用 transactionId：消耗型可以反复购买，同一个 original 下会有很多
	// 笔，用 original 去重等于「第二次买不发币」。
	if got := evt.Purchase.IdempotencyKey(); got != "app_store:2000000100000001" {
		t.Errorf("幂等键应基于 transactionId，实际 %s", got)
	}
}

// 认不出归属的凭证只有两种结局：发给错的人，或者不发。后者能人工补，前者不能撤。
func TestAppleEventRejectsUnattributedReceipt(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()
	for _, tok := range []string{"", "not-a-uuid"} {
		txn, err := VerifyAppleTransaction(sign(map[string]any{"appAccountToken": tok}), now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := AppleEventFrom(txn, "", now); !errors.Is(err, ErrUnknownUser) {
			t.Errorf("appAccountToken=%q 应判 ErrUnknownUser，得到 %v", tok, err)
		}
	}
}

func TestAppleEventStatusFromTransaction(t *testing.T) {
	sign := appleFixture(t)
	now := time.Now()

	// 客户端上报时没有通知类型，状态只能从凭证本身推：过期日已过就是过期。
	expired, err := VerifyAppleTransaction(
		sign(map[string]any{"expiresDate": now.Add(-time.Hour).UnixMilli()}), now)
	if err != nil {
		t.Fatal(err)
	}
	evt, err := AppleEventFrom(expired, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "expired" {
		t.Errorf("过期订阅状态应为 expired，实际 %s", evt.Subscription.Status)
	}

	// 退款/撤销压过一切：哪怕通知说 active，也不能给回权益。
	revoked, err := VerifyAppleTransaction(
		sign(map[string]any{"revocationDate": now.Add(-time.Minute).UnixMilli()}), now)
	if err != nil {
		t.Fatal(err)
	}
	evt, err = AppleEventFrom(revoked, "active", now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "expired" || evt.Subscription.CanceledAt == nil {
		t.Errorf("撤销的订阅应记为 expired 并带 canceled_at：%+v", evt.Subscription)
	}

	// 已退款的消耗型不发币。**也不倒扣** —— 币可能已经花掉，扣成负数会让账户
	// 卡在一个用不了的状态。
	revokedCredits, err := VerifyAppleTransaction(sign(map[string]any{
		"productId":      "squyrrl.credits.p5.once",
		"type":           "Consumable",
		"revocationDate": now.Add(-time.Minute).UnixMilli(),
	}), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppleEventFrom(revokedCredits, "", now); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("已退款的代币不该发放，得到 %v", err)
	}
}

// ─────────────────────────── ASSN V2 通知 ───────────────────────────

// notification 包一层 ASSN V2 信封：外层 signedPayload 是 JWS，里面的
// data.signedTransactionInfo 又是一层 JWS，两层都要验。
func notification(t *testing.T, sign func(any) string, kind string, over map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"signedPayload": sign(map[string]any{
		"notificationType": kind,
		"notificationUUID": "1f2c3d",
		"data": map[string]any{
			"bundleId":              testBundle,
			"environment":           "Production",
			"signedTransactionInfo": sign(txnPayload(over)),
		},
	})})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseAppleNotification(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	sign := func(p any) string { return signAppleJWS(t, leaf.key, x5c, "ES256", p) }
	now := time.Now()

	evt, err := ParseAppleNotification(
		notification(t, sign, "DID_RENEW", nil), prodGuard, now)
	if err != nil {
		t.Fatalf("续期通知应解析成功：%v", err)
	}
	if evt.Subscription == nil || evt.Subscription.Status != "active" {
		t.Fatalf("续期应落成 active 订阅：%+v", evt)
	}

	got, err := ParseAppleNotification(
		notification(t, sign, "EXPIRED", nil), prodGuard, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subscription.Status != "expired" {
		t.Errorf("EXPIRED 应落成 expired，实际 %s", got.Subscription.Status)
	}

	// App Store Connect 上的「发送测试通知」和我们不关心的类型都回 ErrUnknownEvent，
	// handler 据此回 200：苹果对 4xx 会持续重投并最终把端点判为不可达。
	for _, kind := range []string{"TEST", "CONSUMPTION_REQUEST", "RENEWAL_EXTENDED"} {
		if _, err := ParseAppleNotification(
			notification(t, sign, kind, nil), prodGuard, now); !errors.Is(err, ErrUnknownEvent) {
			t.Errorf("%s 应判 ErrUnknownEvent，得到 %v", kind, err)
		}
	}

	// 通知这条路同样过闸门。
	if _, err := ParseAppleNotification(
		notification(t, sign, "DID_RENEW",
			map[string]any{"bundleId": "com.someone.else"}), prodGuard, now); !errors.Is(err, ErrWrongApp) {
		t.Error("别的 App 的通知必须被拒")
	}

	// 未签名 / 乱来的 body 一律拒。
	for _, body := range []string{`{}`, `{"signedPayload":"aaa.bbb.ccc"}`, `not json`} {
		if _, err := ParseAppleNotification([]byte(body), prodGuard, now); err == nil {
			t.Errorf("body=%s 必须被拒", body)
		}
	}
}
