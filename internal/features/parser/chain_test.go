package parser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/pricing"
	"github.com/squyrrl/api/internal/features/wallet"
)

// 这组测试把兜底链、按平台定价、成功才扣费、解析记录放在真库 + 假上游上一起验证。
// 它们回答的都是钱的问题：用户付了多少、平台花了多少、两本账对不对得上。

// chainParser 是一个可配置的测试平台：内置来源可以开、关、失败。
type chainParser struct {
	provider string
	builtin  func() (*ParseResult, error) // nil = 无内置免费来源
	calls    atomic.Int32
}

func (p *chainParser) Provider() string    { return p.provider }
func (p *chainParser) SnippetType() string { return "special" }
func (p *chainParser) Match(uri string) (string, bool) {
	prefix := "https://" + p.provider + ".test/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	return strings.TrimPrefix(uri, prefix), true
}

// builtinChainParser 在 chainParser 之上实现 BuiltinParser。分成两个类型是因为
// 「有没有内置来源」由类型断言决定，不能靠字段开关。
type builtinChainParser struct{ *chainParser }

func (p builtinChainParser) Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error) {
	p.calls.Add(1)
	return p.builtin()
}

func titled(t string) *ParseResult {
	return &ParseResult{Title: &t, Payload: json.RawMessage(`{}`), SourceData: json.RawMessage(`{}`)}
}

// fakeTikHub 按路径返回：/ok 正常数据，/empty 外壳正常但转换后不可用，
// /hidden 抖音审核中作品的实测响应，/down 500。
func fakeTikHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	hidden := `{"code":200,"data":` + string(fixture(t, "douyin_filtered_list")) + `}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"code":200,"data":{"desc":"上游正文"}}`))
		case "/empty":
			_, _ = w.Write([]byte(`{"code":200,"data":{"desc":""}}`))
		case "/hidden":
			_, _ = w.Write([]byte(hidden))
		default:
			http.Error(w, `{"detail":"upstream down"}`, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// testAdapter 指向假上游的某个路径，把 data.desc 当正文。
func testAdapter(provider, url string) Adapter {
	return Adapter{
		Provider: provider,
		Vendor:   extapi.VendorTikHub,
		Request:  func(Target) extapi.Request { return extapi.Request{URL: url} },
		Decode: func(data []byte, _ Target) (*ParseResult, error) {
			var d struct {
				Desc string `json:"desc"`
			}
			if err := json.Unmarshal(data, &d); err != nil {
				return nil, err
			}
			return &ParseResult{Description: &d.Desc, Payload: json.RawMessage(`{}`), SourceData: json.RawMessage(`{}`)}, nil
		},
	}
}

type chainEnv struct {
	pool     *pgxpool.Pool
	provider string
	account  uuid.UUID
	extapi   *extapi.Service
	wallet   *wallet.Service
	adapters AdapterSet
	reg      *Registry
	cache    *Cache
}

// newChainEnv 在真库里建一个专属平台名、一个 TikHub 测试账户，测试结束全部清掉。
func newChainEnv(t *testing.T, p Parser, adapters AdapterSet, balanceMicros int64) *chainEnv {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	provider := p.Provider()

	var account uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO api_accounts (vendor, slug, name) VALUES ('tikhub', $1, 'test') RETURNING id`,
		"test_"+uuid.NewString()).Scan(&account); err != nil {
		t.Fatalf("建账户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM parse_logs WHERE provider = $1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM parse_prices WHERE provider = $1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM api_endpoints WHERE provider = $1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM api_accounts WHERE id = $1`, account)
	})
	if balanceMicros != 0 {
		if _, err := pool.Exec(ctx, `INSERT INTO api_cost_ledger (account_id, delta, reason) VALUES ($1, $2, 'topup')`,
			account, balanceMicros); err != nil {
			t.Fatalf("充值失败: %v", err)
		}
	}

	reg := NewRegistry()
	reg.Register(p)
	return &chainEnv{
		pool: pool, provider: provider, account: account,
		extapi:   extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{TikHub: "test-key"}, adapters),
		wallet:   wallet.NewService(wallet.NewRepo(pool)),
		adapters: adapters, reg: reg,
		cache: newTestCache(t, pool, provider),
	}
}

func (e *chainEnv) addEndpoint(t *testing.T, slug string, unitPriceMicros int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO api_endpoints (account_id, provider, slug, unit_price_micros) VALUES ($1, $2, $3, $4) RETURNING id`,
		e.account, e.provider, slug, unitPriceMicros).Scan(&id); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	return id
}

func (e *chainEnv) service(defaultPrice int64) *Service {
	return NewService(Deps{
		Registry: e.reg, Cache: e.cache, Wallet: e.wallet, Extapi: e.extapi, Adapters: e.adapters,
		Prices: NewPriceRepo(e.pool), Logs: NewLogRepo(e.pool), DefaultPrice: defaultPrice,
	})
}

func (e *chainEnv) user(t *testing.T, credits int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"parse-chain-"+uuid.NewString()+"@example.test").Scan(&uid); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid) })
	if credits > 0 {
		if _, err := e.wallet.Grant(ctx, uid, credits, "test", ""); err != nil {
			t.Fatalf("发代币失败: %v", err)
		}
	}
	return uid
}

func (e *chainEnv) balance(t *testing.T, uid uuid.UUID) int64 {
	t.Helper()
	b, err := e.wallet.Balance(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *chainEnv) lastLog(t *testing.T) LogEntry {
	t.Helper()
	items, err := NewLogRepo(e.pool).List(context.Background(), LogFilter{Provider: e.provider, Limit: 1})
	if err != nil || len(items) == 0 {
		t.Fatalf("取解析记录失败: %v (%d 条)", err, len(items))
	}
	return items[0]
}

func uniqueProvider(name string) string { return "t_" + name + "_" + uuid.NewString()[:8] }

// 免费来源失败后，付费接口按单价从低到高试；一个接口 200 但转换出空内容也算失败、
// 钱照样记上；最终收的是平台售价（最高单价 × 2），与实际由哪个接口产出无关。
func TestChainCheapestFirstChargesPlatformPrice(t *testing.T) {
	srv, _ := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("order")}
	cp.builtin = func() (*ParseResult, error) { return nil, errors.New("内置来源挂了") }
	p := builtinChainParser{cp}

	adapters := AdapterSet{
		"t_cheap_empty": testAdapter(cp.provider, srv.URL+"/empty"),
		"t_pricier_ok":  testAdapter(cp.provider, srv.URL+"/ok"),
	}
	env := newChainEnv(t, p, adapters, 1_000_000)
	// 故意先建贵的：顺序必须由单价决定，而不是建表先后
	env.addEndpoint(t, "t_pricier_ok", 2_000)
	env.addEndpoint(t, "t_cheap_empty", 1_000)
	svc := env.service(10)
	uid := env.user(t, 1_000)

	res, err := svc.Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/r1"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want, _ := pricing.CreditCost(2_000, pricing.DefaultMarginBP) // 40
	if res.CreditsCharged != want {
		t.Errorf("应收平台售价 %d，实收 %d", want, res.CreditsCharged)
	}
	if got := env.balance(t, uid); got != 1_000-want {
		t.Errorf("用户余额 %d，期望 %d", got, 1_000-want)
	}
	if res.Snippet.Description == nil || *res.Snippet.Description != "上游正文" {
		t.Errorf("内容应来自第二个接口: %+v", res.Snippet)
	}

	log := env.lastLog(t)
	if log.Status != LogOK || log.Source != "t_pricier_ok" || log.CreditsCharged != want {
		t.Errorf("记录不对: %+v", log)
	}
	if log.CostMicros != 3_000 {
		t.Errorf("成本应为两次计费调用之和 3000，实为 %d", log.CostMicros)
	}
	gotSeq := []string{}
	for _, a := range log.Attempts {
		gotSeq = append(gotSeq, a.Source+":"+a.Outcome)
	}
	wantSeq := "builtin:" + cp.provider + ":failed,t_cheap_empty:failed,t_pricier_ok:ok"
	if strings.Join(gotSeq, ",") != wantSeq {
		t.Errorf("尝试顺序\n got  %s\n want %s", strings.Join(gotSeq, ","), wantSeq)
	}
	if log.Attempts[1].ErrorKind != extapi.KindDecode || log.Attempts[1].CostMicros != 1_000 {
		t.Errorf("空内容那次应是已计费的 decode 失败: %+v", log.Attempts[1])
	}
	if b := log.Attempts[2].BalanceAfter; b == nil || *b != 1_000_000-3_000 {
		t.Errorf("余额快照应为 997000，实为 %v", b)
	}

	// 账户流水与解析记录的成本对得上
	var spent int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT -SUM(delta) FROM api_cost_ledger WHERE account_id = $1 AND reason = 'call'`, env.account).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent != log.CostMicros {
		t.Errorf("账户流水成本 %d 与解析记录 %d 对不上", spent, log.CostMicros)
	}
}

// 免费来源成功就到此为止：付费接口一个都不碰，但照样收平台售价。
func TestChainFreeSourceFirst(t *testing.T) {
	srv, hits := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("free")}
	cp.builtin = func() (*ParseResult, error) { return titled("免费内容"), nil }
	env := newChainEnv(t, builtinChainParser{cp}, AdapterSet{"t_paid": testAdapter(cp.provider, srv.URL+"/ok")}, 1_000_000)
	env.addEndpoint(t, "t_paid", 1_000)
	uid := env.user(t, 100)

	res, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/x"})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("免费来源成功后不应调付费接口，实调 %d 次", hits.Load())
	}
	if res.CreditsCharged != 20 { // 售价仍按已接入接口的最高单价算
		t.Errorf("应收 20，实收 %d", res.CreditsCharged)
	}
	if log := env.lastLog(t); log.CostMicros != 0 || log.Source != "builtin:"+cp.provider {
		t.Errorf("记录不对: %+v", log)
	}
}

// 整条链失败：分文不取，用户只看到通用原因，细节进解析记录。
func TestChainFailureChargesNothing(t *testing.T) {
	srv, _ := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("fail")}
	env := newChainEnv(t, cp, AdapterSet{"t_down": testAdapter(cp.provider, srv.URL+"/down")}, 1_000_000)
	env.addEndpoint(t, "t_down", 1_000)
	env.addEndpoint(t, "t_no_code", 500) // 库里有、代码里没有适配器
	uid := env.user(t, 100)

	_, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/x"})
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("应返回通用失败，得到 %v", err)
	}
	if got := env.balance(t, uid); got != 100 {
		t.Errorf("失败不应扣费，余额 %d", got)
	}
	log := env.lastLog(t)
	if log.Status != LogFailed || log.CreditsCharged != 0 || len(log.Attempts) != 2 {
		t.Fatalf("记录不对: %+v", log)
	}
	if a := log.Attempts[0]; a.Source != "t_no_code" || a.Outcome != AttemptSkipped || a.ErrorKind != extapi.KindNoAdapter {
		t.Errorf("没有适配器的接口应被跳过并记下原因: %+v", a)
	}
	if a := log.Attempts[1]; a.ErrorKind != extapi.KindHTTPStatus || a.HTTPStatus != 500 || a.CostMicros != 0 {
		t.Errorf("500 应记为不计费的 http_status: %+v", a)
	}
}

// 余额不够付售价：直接 402，不碰上游也不碰缓存。
func TestChainInsufficientCreditsSkipsUpstream(t *testing.T) {
	srv, hits := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("poor")}
	env := newChainEnv(t, cp, AdapterSet{"t_ok": testAdapter(cp.provider, srv.URL+"/ok")}, 1_000_000)
	env.addEndpoint(t, "t_ok", 1_000)
	uid := env.user(t, 5)

	_, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/x"})
	var ie *wallet.InsufficientCreditsError
	if !errors.As(err, &ie) || ie.Required != 20 || ie.Balance != 5 {
		t.Fatalf("应返回带数字的余额不足，得到 %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("付不起时不应调上游，实调 %d 次", hits.Load())
	}
}

// 账户没钱的接口被跳过，链继续往后走。
func TestChainSkipsDrainedAccount(t *testing.T) {
	srv, hits := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("drained")}
	env := newChainEnv(t, cp, AdapterSet{"t_ok": testAdapter(cp.provider, srv.URL+"/ok")}, 0)
	env.addEndpoint(t, "t_ok", 1_000)
	uid := env.user(t, 100)

	_, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/x"})
	if !errors.Is(err, ErrParseFailed) || hits.Load() != 0 {
		t.Fatalf("余额为 0 的账户不应被调用：err=%v hits=%d", err, hits.Load())
	}
	if a := env.lastLog(t).Attempts[0]; a.Outcome != AttemptSkipped || a.ErrorKind != extapi.KindNoBalance {
		t.Errorf("应记为余额耗尽跳过: %+v", a)
	}
}

// 一个接口确认作品本身不可见（抖音审核中）：后面的接口不再调用、记成跳过，
// 记录的总原因写明不可见，成本只有那一次调用。
func TestChainStopsWhenItemHidden(t *testing.T) {
	srv, hits := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("hidden")}
	hidden := testAdapter(cp.provider, srv.URL+"/hidden")
	hidden.Decode = TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"].Decode
	env := newChainEnv(t, cp, AdapterSet{"t_hidden": hidden, "t_ok": testAdapter(cp.provider, srv.URL+"/ok")}, 1_000_000)
	env.addEndpoint(t, "t_hidden", 1_000)
	env.addEndpoint(t, "t_ok", 2_000)
	uid := env.user(t, 100)

	_, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/7698348670932978600"})
	if !errors.Is(err, ErrItemHidden) {
		t.Fatalf("应返回作品不可见，得到 %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("确认不可见后不应再调上游，实调 %d 次", hits.Load())
	}
	log := env.lastLog(t)
	if log.Status != LogFailed || log.CostMicros != 1_000 || !strings.Contains(log.Error, "审核中，仅作者可见") || len(log.Attempts) != 2 {
		t.Fatalf("记录不对: %+v", log)
	}
	if a := log.Attempts[0]; a.Outcome != AttemptFailed || a.ErrorKind != extapi.KindUnavailable {
		t.Errorf("第一个接口应记为作品不可见: %+v", a)
	}
	if a := log.Attempts[1]; a.Source != "t_ok" || a.Outcome != AttemptSkipped || a.ErrorKind != extapi.KindUnavailable || a.CostMicros != 0 {
		t.Errorf("后面的接口应记为因不可见跳过: %+v", a)
	}
}

// 确认不可见之后，同一作品的普通请求（换个用户也一样）不再调上游、不扣费；
// 强制刷新照常重取，取到内容就删掉记录，之后的普通请求回到缓存。
func TestHiddenItemRemembered(t *testing.T) {
	srv, hits := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("gone")}
	var visible atomic.Bool // 作品是否又能看了：决定假上游回哪一份
	ok, hiddenDecode := testAdapter(cp.provider, srv.URL+"/ok"), TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"].Decode
	flip := Adapter{
		Provider: cp.provider, Vendor: extapi.VendorTikHub,
		Request: func(tg Target) extapi.Request {
			if visible.Load() {
				return ok.Request(tg)
			}
			return extapi.Request{URL: srv.URL + "/hidden"}
		},
		Decode: func(data []byte, tg Target) (*ParseResult, error) {
			if visible.Load() {
				return ok.Decode(data, tg)
			}
			return hiddenDecode(data, tg)
		},
	}
	env := newChainEnv(t, cp, AdapterSet{"t_flip": flip}, 1_000_000)
	env.addEndpoint(t, "t_flip", 1_000)
	svc := env.service(10)
	a, b := env.user(t, 100), env.user(t, 100)
	ctx := context.Background()
	uri := "https://" + cp.provider + ".test/7698348670932978600"

	if _, err := svc.Parse(ctx, a, ParseInput{URI: uri}); !errors.Is(err, ErrItemHidden) {
		t.Fatalf("首次应确认不可见，得到 %v", err)
	}
	if _, err := svc.Parse(ctx, b, ParseInput{URI: uri}); !errors.Is(err, ErrItemHidden) {
		t.Fatalf("记下之后应直接回不可见，得到 %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("记下之后不应再调上游，实调 %d 次", hits.Load())
	}
	log := env.lastLog(t)
	if log.Status != LogFailed || log.CostMicros != 0 || len(log.Attempts) != 0 || !strings.Contains(log.Error, "未调上游") {
		t.Fatalf("命中记录的那笔应是无成本、无尝试的失败: %+v", log)
	}

	// 强制刷新不受记录挡：仍不可见就再确认一次
	if _, err := svc.Parse(ctx, b, ParseInput{URI: uri, Force: true}); !errors.Is(err, ErrItemHidden) || hits.Load() != 2 {
		t.Fatalf("强制刷新应照常重取：err=%v hits=%d", err, hits.Load())
	}

	visible.Store(true)
	if _, err := svc.Parse(ctx, b, ParseInput{URI: uri, Force: true}); err != nil || hits.Load() != 3 {
		t.Fatalf("作品恢复后强制刷新应取到内容：err=%v hits=%d", err, hits.Load())
	}
	if _, hidden, err := env.cache.Hidden(ctx, cp.provider, "7698348670932978600"); err != nil || hidden {
		t.Fatalf("取到内容后记录应删除：hidden=%v err=%v", hidden, err)
	}
	resp, err := svc.Parse(ctx, a, ParseInput{URI: uri})
	if err != nil || !resp.Cached || hits.Load() != 3 {
		t.Fatalf("之后的普通请求应命中缓存：err=%v resp=%+v hits=%d", err, resp, hits.Load())
	}
	if got := env.balance(t, a); got != 80 {
		t.Errorf("只有取到内容的那次扣费，a 余额应为 80，实为 %d", got)
	}
	if got := env.balance(t, b); got != 80 {
		t.Errorf("只有取到内容的那次扣费，b 余额应为 80，实为 %d", got)
	}
}

// 后台设的售价覆盖自动价；命中缓存收同一个价。
func TestPriceOverrideAppliesToCacheHits(t *testing.T) {
	cp := &chainParser{provider: uniqueProvider("override")}
	cp.builtin = func() (*ParseResult, error) { return titled("内容"), nil }
	env := newChainEnv(t, builtinChainParser{cp}, AdapterSet{}, 0)
	svc := env.service(10)
	if err := svc.SetPrice(context.Background(), cp.provider, 33); err != nil {
		t.Fatal(err)
	}
	uid := env.user(t, 100)
	uri := "https://" + cp.provider + ".test/x"

	first, err := svc.Parse(context.Background(), uid, ParseInput{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Parse(context.Background(), uid, ParseInput{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached || first.CreditsCharged != 33 || second.CreditsCharged != 33 {
		t.Errorf("首次与命中缓存都应收 33：first=%d second=%d cached=%v",
			first.CreditsCharged, second.CreditsCharged, second.Cached)
	}
	if got := env.balance(t, uid); got != 100-66 {
		t.Errorf("余额 %d，期望 34", got)
	}
	if log := env.lastLog(t); !log.Cached || log.CostMicros != 0 || log.RevenueMicros != 33*pricing.MicrosPerCredit {
		t.Errorf("缓存命中的记录不对: %+v", log)
	}

	prices, err := svc.Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 1 || prices[0].OverrideCredits == nil || *prices[0].OverrideCredits != 33 ||
		prices[0].AutoCredits != 10 || prices[0].EffectiveCredits != 33 {
		t.Errorf("定价页数据不对: %+v", prices)
	}
}

// 售价低于最贵接口单价时，定价页要标出来：兜底落到它时每次都亏。
func TestPricesFlagLoss(t *testing.T) {
	cp := &chainParser{provider: uniqueProvider("loss")}
	env := newChainEnv(t, cp, AdapterSet{"t_x": testAdapter(cp.provider, "http://unused")}, 0)
	env.addEndpoint(t, "t_x", 5_000) // $0.005 = 50 credits 的成本
	svc := env.service(10)
	if err := svc.SetPrice(context.Background(), cp.provider, 40); err != nil {
		t.Fatal(err)
	}
	prices, err := svc.Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !prices[0].Loss || prices[0].AutoCredits != 100 {
		t.Errorf("40 credits 盖不住 $0.005 的接口，应标亏损: %+v", prices[0])
	}
}

// 并发合并时只有发起调用的那个请求记成本，其余标 shared、成本为 0。
func TestChainSharedRequestsCountCostOnce(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		_, _ = w.Write([]byte(`{"code":200,"data":{"desc":"热门"}}`))
	}))
	t.Cleanup(srv.Close)
	cp := &chainParser{provider: uniqueProvider("shared")}
	env := newChainEnv(t, cp, AdapterSet{"t_hot": testAdapter(cp.provider, srv.URL)}, 1_000_000)
	env.addEndpoint(t, "t_hot", 1_000)
	svc := env.service(10)
	uri := "https://" + cp.provider + ".test/hot"

	const n = 4
	errs := make(chan error, n)
	for range n {
		uid := env.user(t, 100)
		go func() {
			_, err := svc.Parse(context.Background(), uid, ParseInput{URI: uri})
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(block)
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	items, err := NewLogRepo(env.pool).List(context.Background(), LogFilter{Provider: cp.provider})
	if err != nil {
		t.Fatal(err)
	}
	var cost, leaders, revenue int64
	for _, it := range items {
		cost += it.CostMicros
		revenue += it.CreditsCharged
		if !it.Shared && !it.Cached {
			leaders++
		}
	}
	if leaders != 1 || cost != 1_000 || revenue != n*20 {
		t.Errorf("应只有 1 个发起者承担 1000 成本、%d 个请求各收 20：leaders=%d cost=%d revenue=%d",
			n, leaders, cost, revenue)
	}
}

// fakeResolver 把短链按表换成作品链接，不走网络。
type fakeResolver map[string]string

func (f fakeResolver) Resolve(_ context.Context, uri string, done func(string) bool) (string, error) {
	if to, ok := f[uri]; ok && done(to) {
		return to, nil
	}
	return "", errShortLink
}

// 短链 → 跟跳转 → 按作品 ID 解析；不适用的接口被跳过、不花钱。
// 短链解析失败不扣费，记一笔失败。用测试专属平台，不碰库里真实平台的种子接口。
func TestShortLinkResolvesBeforeParsing(t *testing.T) {
	srv, hits := fakeTikHub(t)
	plat := &platform{id: uniqueProvider("short"),
		canonical: res(`notes\.test/item/([0-9a-f]{24})`),
		short:     res(`(?:^|//)s\.notes\.test/[a-z]+`)}

	video := testAdapter(plat.id, srv.URL+"/ok")
	video.Accept = xhsMaybeVideo
	adapters := AdapterSet{"t_short_video": video, "t_short_image": testAdapter(plat.id, srv.URL+"/ok")}
	env := newChainEnv(t, plat, adapters, 1_000_000)
	env.addEndpoint(t, "t_short_video", 10_000)
	env.addEndpoint(t, "t_short_image", 10_000)
	svc := NewService(Deps{
		Registry: env.reg, Cache: env.cache, Wallet: env.wallet, Extapi: env.extapi, Adapters: adapters,
		Logs: NewLogRepo(env.pool), DefaultPrice: 10,
		Resolver: fakeResolver{"https://s.notes.test/abc": "https://notes.test/item/697c0eee000000000a03c308?type=normal"},
	})
	uid := env.user(t, 1_000)
	ctx := context.Background()

	res, err := svc.Parse(ctx, uid, ParseInput{URI: "https://s.notes.test/abc"})
	if err != nil {
		t.Fatalf("短链解析失败: %v", err)
	}
	if res.ResourceID != "697c0eee000000000a03c308" || res.CreditsCharged != 200 {
		t.Errorf("应按作品 ID 解析并收 200：%+v", res)
	}
	if hits.Load() != 1 {
		t.Errorf("type=normal 时视频接口应被跳过，只调一次上游，实调 %d", hits.Load())
	}
	log := env.lastLog(t)
	if a := log.Attempts[0]; a.Outcome != AttemptSkipped || a.Source != "t_short_video" || a.CostMicros != 0 {
		t.Errorf("视频接口应记为跳过且不花钱: %+v", a)
	}

	before := env.balance(t, uid)
	if _, err := svc.Parse(ctx, uid, ParseInput{URI: "https://s.notes.test/dead"}); !errors.Is(err, ErrParseFailed) {
		t.Fatalf("短链解析失败应返回通用失败，得到 %v", err)
	}
	if env.balance(t, uid) != before {
		t.Error("短链解析失败不应扣费")
	}
	if log := env.lastLog(t); log.Status != LogFailed || !strings.Contains(log.Error, "短链") {
		t.Errorf("应记一笔短链失败: %+v", log)
	}
}

// 平台按链接给售价乘倍数（小红书网页端链接 2 倍）：余额预检与实际扣费都按乘过的价。
func TestPriceFactorDoublesCharge(t *testing.T) {
	srv, _ := fakeTikHub(t)
	plat := &platform{id: uniqueProvider("factor"), canonical: res(`factor\.test/item/(\w+)`),
		priceFactor: func(t Target) int64 {
			if queryParam(t.Resolved, "type") == "" {
				return 2
			}
			return 1
		}}
	env := newChainEnv(t, plat, AdapterSet{"t_f": testAdapter(plat.id, srv.URL+"/ok")}, 1_000_000)
	env.addEndpoint(t, "t_f", 1_000) // 自动价 20
	svc := env.service(10)
	ctx := context.Background()

	uid := env.user(t, 100)
	typed, err := svc.Parse(ctx, uid, ParseInput{URI: "https://factor.test/item/a?type=normal"})
	if err != nil || typed.CreditsCharged != 20 {
		t.Fatalf("带 type 的链接按原价 20：%+v %v", typed, err)
	}
	untyped, err := svc.Parse(ctx, uid, ParseInput{URI: "https://factor.test/item/b"})
	if err != nil || untyped.CreditsCharged != 40 {
		t.Fatalf("不带 type 的链接按 2 倍 40：%+v %v", untyped, err)
	}

	poor := env.user(t, 30) // 够原价、不够 2 倍
	_, err = svc.Parse(ctx, poor, ParseInput{URI: "https://factor.test/item/c"})
	var ie *wallet.InsufficientCreditsError
	if !errors.As(err, &ie) || ie.Required != 40 {
		t.Errorf("余额预检应按 2 倍价：%v", err)
	}
}

// 解析记录的每一步带上当次调用的线上采样，排查时能点回原始响应。
func TestAttemptLinksLiveSample(t *testing.T) {
	srv, _ := fakeTikHub(t)
	cp := &chainParser{provider: uniqueProvider("sample")}
	env := newChainEnv(t, cp, AdapterSet{"t_s": testAdapter(cp.provider, srv.URL+"/ok")}, 1_000_000)
	id := env.addEndpoint(t, "t_s", 1_000)
	if _, err := env.pool.Exec(context.Background(), `UPDATE api_endpoints SET config = '{"archive_live": true}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	uid := env.user(t, 100)
	if _, err := env.service(10).Parse(context.Background(), uid, ParseInput{URI: "https://" + cp.provider + ".test/x"}); err != nil {
		t.Fatal(err)
	}
	a := env.lastLog(t).Attempts[0]
	if a.SampleID == nil {
		t.Fatal("开了线上采样的接口，尝试应关联采样")
	}
	sample, err := env.extapi.GetSample(context.Background(), *a.SampleID)
	if err != nil || !strings.Contains(string(sample.Response), "上游正文") {
		t.Errorf("关联的采样不对: %+v %v", sample, err)
	}
}
