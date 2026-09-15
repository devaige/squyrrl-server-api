package subscriptions

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	playPkg   = "com.squyrrl.android"
	playUser  = "33333333-3333-3333-3333-333333333333"
	playToken = "abcdefghijklmnop.AO-J1Ox"
)

var playGuardProd = PlayGuard{PackageName: playPkg}

// newStubPlay 起一个假的 Play Developer API，并把 PlayAPI 指过去。
//
// 连 OAuth 换 token 那一跳一起假掉而不是预置一个 token：JWT-bearer 断言是
// 这条路上唯一一段我们自己签的东西，出错时的表现是 Google 回 invalid_grant，
// 从日志里根本看不出是哪一段字段写错了。让它在测试里真的跑一遍。
func newStubPlay(t *testing.T, handler http.HandlerFunc) (*PlayAPI, *httptest.Server, *int64) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var tokenHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt64(&tokenHits, 1)
			if err := r.ParseForm(); err != nil {
				t.Errorf("token 请求不是表单: %v", err)
			}
			if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("grant_type = %q", got)
			}
			assertPlayAssertion(t, r.PostForm.Get("assertion"))
			_, _ = w.Write([]byte(`{"access_token":"ya29.stub","expires_in":3600}`))
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ya29.stub" {
			t.Errorf("API 调用没带上 access token: %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	api := &PlayAPI{
		guard:    playGuardProd,
		email:    "squyrrl@squyrrl.iam.gserviceaccount.com",
		key:      key,
		tokenURI: srv.URL + "/token",
		base:     srv.URL,
		http:     srv.Client(),
		now:      time.Now,
	}
	return api, srv, &tokenHits
}

func assertPlayAssertion(t *testing.T, assertion string) {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("断言不是三段式: %q", assertion)
	}
	var h struct{ Alg, Typ string }
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	if h.Alg != "RS256" {
		t.Errorf("alg = %q，服务账号断言只能是 RS256", h.Alg)
	}
	var c struct {
		Iss, Scope, Aud string
		Exp, Iat        int64
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.Iss, ".iam.gserviceaccount.com") {
		t.Errorf("iss = %q", c.Iss)
	}
	if c.Scope != playScope {
		t.Errorf("scope = %q", c.Scope)
	}
	if !strings.HasSuffix(c.Aud, "/token") {
		t.Errorf("aud 必须是 token 端点本身，得到 %q", c.Aud)
	}
	if c.Exp <= c.Iat {
		t.Errorf("exp(%d) 不晚于 iat(%d)", c.Exp, c.Iat)
	}
}

func subJSON(t *testing.T, over map[string]any) string {
	t.Helper()
	body := map[string]any{
		"subscriptionState": "SUBSCRIPTION_STATE_ACTIVE",
		"startTime":         time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
		"latestOrderId":     "GPA.3300-0000-0000-00001",
		"externalAccountIdentifiers": map[string]any{
			"obfuscatedExternalAccountId": playUser,
		},
		"lineItems": []map[string]any{{
			"productId":  "squyrrl_plan_basic_monthly",
			"expiryTime": time.Now().Add(20 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}},
	}
	for k, v := range over {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func productJSON(t *testing.T, over map[string]any) string {
	t.Helper()
	body := map[string]any{
		"purchaseTimeMillis":          fmt.Sprint(time.Now().UnixMilli()),
		"purchaseState":               0,
		"consumptionState":            0,
		"orderId":                     "GPA.3300-0000-0000-00002",
		"quantity":                    1,
		"obfuscatedExternalAccountId": playUser,
	}
	for k, v := range over {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func serve(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func TestPlayAccessTokenIsReusedAcrossCalls(t *testing.T) {
	api, _, hits := newStubPlay(t, serve(subJSON(t, nil)))
	for i := 0; i < 3; i++ {
		if _, err := api.Subscription(context.Background(), playToken); err != nil {
			t.Fatalf("第 %d 次查询: %v", i, err)
		}
	}
	// 每次收单都换一次 token，等于给每笔购买加一次往返和一次失败面。
	if got := *hits; got != 1 {
		t.Errorf("换 token 次数 = %d，想要 1（应当缓存）", got)
	}
}

func TestPlaySubscriptionURLCarriesPackageAndToken(t *testing.T) {
	var gotPath string
	api, _, _ := newStubPlay(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(subJSON(t, nil)))
	})
	if _, err := api.Subscription(context.Background(), playToken); err != nil {
		t.Fatal(err)
	}
	// 包名在路径里 —— 别的应用的 token 拿过来会 404，而不是「验完签再比对」。
	if !strings.Contains(gotPath, playPkg) {
		t.Errorf("路径里没有包名: %s", gotPath)
	}
	if !strings.Contains(gotPath, "subscriptionsv2") {
		t.Errorf("订阅必须查 v2 接口（v1 不给 obfuscatedExternalAccountId）: %s", gotPath)
	}
}

// 这张表是整条路上最要紧的一处分歧：「这笔单不成立」要客户端了结掉交易，
// 「我们暂时问不到」绝不能了结。把后者当前者，等于一次网络抖动就吞掉一笔付款。
func TestPlayHTTPStatusSplitsPermanentFromTransient(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{http.StatusNotFound, ErrUnknownPurchase},
		{http.StatusGone, ErrUnknownPurchase},
		{http.StatusBadRequest, ErrUnknownPurchase},
		{http.StatusUnauthorized, ErrPlayUnconfigured},
		{http.StatusForbidden, ErrPlayUnconfigured},
		{http.StatusInternalServerError, ErrStoreUnavailable},
		{http.StatusBadGateway, ErrStoreUnavailable},
		{http.StatusServiceUnavailable, ErrStoreUnavailable},
	}
	for _, c := range cases {
		api, _, _ := newStubPlay(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.code)
		})
		_, err := api.Subscription(context.Background(), playToken)
		if !errors.Is(err, c.want) {
			t.Errorf("HTTP %d → %v，想要 %v", c.code, err, c.want)
		}
	}
}

func TestGoogleRedeemSubscription(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, nil)))
	evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := evt.Subscription
	if s == nil {
		t.Fatal("订阅商品却解出了一次性支付")
	}
	if s.UserID.String() != playUser {
		t.Errorf("user = %s", s.UserID)
	}
	if s.Provider != ProviderGoogle || s.Tier != "basic" || s.BillingPeriod != "monthly" {
		t.Errorf("SKU 解错了: %+v", s)
	}
	if s.Status != "active" {
		t.Errorf("status = %s", s.Status)
	}
	// 周期末尾必须来自 API，而不是 now + 30 天那种推算。
	if s.PeriodEnd.Before(time.Now().Add(19 * 24 * time.Hour)) {
		t.Errorf("period_end = %v，没取到 expiryTime", s.PeriodEnd)
	}
	if s.ProviderSubscriptionID != playToken {
		t.Errorf("订阅键必须是 purchaseToken，得到 %q", s.ProviderSubscriptionID)
	}
}

// 用户在 Play 里关掉自动续期，订阅到期前照常有效 —— 那段时间的钱已经付了。
// 映射成 canceled 会让 quota 当场掐断权益。
func TestGoogleRedeemCanceledKeepsEntitlementUntilExpiry(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"subscriptionState": "SUBSCRIPTION_STATE_CANCELED",
	})))
	evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "active" {
		t.Errorf("status = %s，已关续期但未到期仍应当是 active", evt.Subscription.Status)
	}
	if evt.Subscription.CanceledAt == nil {
		t.Error("canceled_at 应当记下来")
	}
}

func TestGoogleRedeemActiveButExpiredReadsExpired(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"lineItems": []map[string]any{{
			"productId":  "squyrrl_plan_basic_monthly",
			"expiryTime": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}},
	})))
	evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Status != "expired" {
		t.Errorf("status = %s，expiryTime 已过应当是 expired", evt.Subscription.Status)
	}
}

func TestGoogleRedeemSubscriptionStates(t *testing.T) {
	cases := map[string]string{
		"SUBSCRIPTION_STATE_IN_GRACE_PERIOD": "past_due",
		"SUBSCRIPTION_STATE_ON_HOLD":         "past_due",
		"SUBSCRIPTION_STATE_PAUSED":          "past_due",
		"SUBSCRIPTION_STATE_EXPIRED":         "expired",
	}
	for state, want := range cases {
		api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{"subscriptionState": state})))
		evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
			"squyrrl_plan_basic_monthly", playToken, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if evt.Subscription.Status != want {
			t.Errorf("%s → %s，想要 %s", state, evt.Subscription.Status, want)
		}
	}
}

// 待付款（如巴西 boleto）不落库：钱还没到账。付成了会再来一条 RTDN。
func TestGoogleRedeemPendingIsIgnored(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"subscriptionState": "SUBSCRIPTION_STATE_PENDING",
	})))
	_, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v，想要 ErrUnknownEvent", err)
	}
}

// 商品号以 API 返回的 lineItems 为准，客户端报什么不算数。
func TestGoogleRedeemIgnoresClientClaimedTier(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(subJSON(t, map[string]any{
		"lineItems": []map[string]any{{
			"productId":  "squyrrl_plan_basic_monthly",
			"expiryTime": time.Now().Add(20 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}},
	})))
	evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_maximum_yearly", playToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subscription.Tier != "basic" || evt.Subscription.BillingPeriod != "monthly" {
		t.Errorf("客户端谎报的档位被采信了: %+v", evt.Subscription)
	}
}

func TestGoogleRedeemWithoutAccountBindingIsRejected(t *testing.T) {
	for _, over := range []map[string]any{
		{"externalAccountIdentifiers": nil},
		{"externalAccountIdentifiers": map[string]any{"obfuscatedExternalAccountId": ""}},
		{"externalAccountIdentifiers": map[string]any{"obfuscatedExternalAccountId": "not-a-uuid"}},
	} {
		api, _, _ := newStubPlay(t, serve(subJSON(t, over)))
		_, err := GoogleRedeem(context.Background(), api, playGuardProd,
			"squyrrl_plan_basic_monthly", playToken, time.Now())
		if !errors.Is(err, ErrUnknownUser) {
			t.Errorf("%v → %v，想要 ErrUnknownUser", over, err)
		}
	}
}

// 许可测试名单里的账号买东西不扣款，凭证却完全正常。
func TestGoogleRedeemRejectsTestPurchaseInProduction(t *testing.T) {
	body := subJSON(t, map[string]any{"testPurchase": map[string]any{}})
	api, _, _ := newStubPlay(t, serve(body))
	_, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if !errors.Is(err, ErrWrongEnvironment) {
		t.Fatalf("生产环境放行了测试单: %v", err)
	}

	api2, _, _ := newStubPlay(t, serve(body))
	testGuard := PlayGuard{PackageName: playPkg, AllowTest: true}
	if _, err := GoogleRedeem(context.Background(), api2, testGuard,
		"squyrrl_plan_basic_monthly", playToken, time.Now()); err != nil {
		t.Errorf("AllowTest 下仍被拒: %v", err)
	}
}

func TestGoogleRedeemCredits(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(productJSON(t, map[string]any{"quantity": 3})))
	evt, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_credits_p5_once", playToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := evt.Purchase
	if p == nil {
		t.Fatal("代币商品却解出了订阅")
	}
	if p.Credits != 55_000*3 {
		t.Errorf("credits = %d，想要 %d（$5 档 × 3 份）", p.Credits, 55_000*3)
	}
	// 幂等键必须是每笔订单各不相同的 orderId：消耗型可以反复买。
	if p.IdempotencyKey() != "google_play:GPA.3300-0000-0000-00002" {
		t.Errorf("幂等键 = %q", p.IdempotencyKey())
	}
}

func TestGoogleRedeemCreditsQueriesProductEndpoint(t *testing.T) {
	var gotPath string
	api, _, _ := newStubPlay(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(productJSON(t, nil)))
	})
	if _, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_credits_p1_once", playToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 商品号是路径的一部分 —— 拿订阅的 token 配代币的商品号，Play 直接 404。
	if !strings.Contains(gotPath, "/products/squyrrl_credits_p1_once/") {
		t.Errorf("路径 = %s", gotPath)
	}
}

func TestGoogleRedeemCreditsNonPurchasedStates(t *testing.T) {
	for _, state := range []int{1, 2} { // 1 已取消 / 2 待处理
		api, _, _ := newStubPlay(t, serve(productJSON(t, map[string]any{"purchaseState": state})))
		_, err := GoogleRedeem(context.Background(), api, playGuardProd,
			"squyrrl_credits_p5_once", playToken, time.Now())
		if !errors.Is(err, ErrUnknownEvent) {
			t.Errorf("purchaseState=%d → %v，想要 ErrUnknownEvent", state, err)
		}
	}
}

// purchaseType 缺席才是正常购买；0 是许可测试单，而 0 恰好是 int 的零值 ——
// 用值类型解这个字段会把每一笔正常购买都判成测试单。
func TestGoogleRedeemCreditsTestPurchase(t *testing.T) {
	api, _, _ := newStubPlay(t, serve(productJSON(t, map[string]any{"purchaseType": 0})))
	_, err := GoogleRedeem(context.Background(), api, playGuardProd,
		"squyrrl_credits_p5_once", playToken, time.Now())
	if !errors.Is(err, ErrWrongEnvironment) {
		t.Errorf("err = %v，想要 ErrWrongEnvironment", err)
	}

	api2, _, _ := newStubPlay(t, serve(productJSON(t, nil)))
	if _, err := GoogleRedeem(context.Background(), api2, playGuardProd,
		"squyrrl_credits_p5_once", playToken, time.Now()); err != nil {
		t.Errorf("正常购买（purchaseType 缺席）被判成测试单: %v", err)
	}
}

func TestPlayAPINotConfigured(t *testing.T) {
	var nilAPI *PlayAPI
	if nilAPI.Ready() {
		t.Error("未配置却 Ready")
	}
	_, err := GoogleRedeem(context.Background(), nilAPI, playGuardProd,
		"squyrrl_plan_basic_monthly", playToken, time.Now())
	if !errors.Is(err, ErrPlayUnconfigured) {
		t.Errorf("err = %v，想要 ErrPlayUnconfigured", err)
	}

	api, err := NewPlayAPI(playGuardProd, "")
	if err != nil || api.Ready() {
		t.Errorf("空凭据应当得到未启用而非报错: %v", err)
	}
	if _, err := NewPlayAPI(PlayGuard{}, `{"client_email":"a","private_key":"b"}`); err != nil {
		t.Errorf("没配包名时不该报错，只是不启用: %v", err)
	}
	if _, err := NewPlayAPI(playGuardProd, `{"client_email":"a@b.iam.gserviceaccount.com"}`); err == nil {
		t.Error("缺私钥应当报错")
	}
}
