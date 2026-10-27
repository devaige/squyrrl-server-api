package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/tg"
)

func TestAdminCacheInvalidationValidatesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/internal/admin", tg.InternalAuth("test-internal-token"))
	NewHandler(nil).RegisterInternal(g)

	for _, tc := range []struct {
		name, path, token string
		status            int
	}{
		{"invalid id", "/internal/admin/parse-logs/not-a-uuid/cache", "test-internal-token", http.StatusBadRequest},
		{"missing token", "/internal/admin/parse-logs/" + uuid.NewString() + "/cache", "", http.StatusUnauthorized},
		{"wrong token", "/internal/admin/parse-logs/" + uuid.NewString() + "/cache", "wrong", http.StatusUnauthorized},
		{"public path", "/uris/parse-logs/" + uuid.NewString() + "/cache", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			req.Header.Set("X-Internal-Token", tc.token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("得到 %d，期望 %d：%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

// 从历史记录失效作品缓存：不重新解析、不删历史，下一次普通解析才重取。
func TestAdminCacheInvalidationRefetchesOnNextParse(t *testing.T) {
	p := &countingParser{version: "v1"}
	svc := newTestService(t, p, 0)
	ctx := context.Background()
	svc.logs = NewLogRepo(svc.cache.pool)
	t.Cleanup(func() {
		_, _ = svc.cache.pool.Exec(ctx, `DELETE FROM parse_logs WHERE provider = $1`, p.Provider())
	})
	const resourceID = "channel/123"
	const uri = "https://example.test/" + resourceID

	for _, version := range []string{"v0", "v1"} {
		if err := svc.cache.Put(ctx, p.Provider(), resourceID, version, snip("旧数据"), 0); err != nil {
			t.Fatal(err)
		}
	}
	entry := &LogEntry{Provider: p.Provider(), ResourceID: resourceID, Status: LogOK, Cached: true}
	if err := svc.logs.Write(ctx, entry); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	NewHandler(svc).RegisterInternal(r.Group("/internal/admin"))
	call := func(id uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/internal/admin/parse-logs/"+id.String()+"/cache", nil))
		return w
	}
	for _, wantRemoved := range []int64{2, 0} {
		w := call(entry.ID)
		var out struct {
			Removed int64 `json:"removed"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || out.Removed != wantRemoved {
			t.Fatalf("失效失败：status=%d body=%s err=%v", w.Code, w.Body.String(), err)
		}
	}
	if p.calls.Load() != 0 {
		t.Fatal("失效操作不应调用上游")
	}
	if w := call(uuid.New()); w.Code != http.StatusNotFound {
		t.Fatalf("不存在的记录应 404：%d %s", w.Code, w.Body.String())
	}
	if provider, id, err := svc.logs.resource(ctx, entry.ID); err != nil || provider != p.Provider() || id != resourceID {
		t.Fatalf("失效不应修改历史记录：provider=%q resource=%q err=%v", provider, id, err)
	}

	uid := uuid.New()
	for _, wantCached := range []bool{false, true} {
		res, err := svc.Parse(ctx, uid, ParseInput{URI: uri})
		if err != nil || res.Cached != wantCached || p.calls.Load() != 1 {
			t.Fatalf("下一次解析应重取，之后命中新缓存：response=%+v calls=%d err=%v", res, p.calls.Load(), err)
		}
		if res.Snippet.Title == nil || *res.Snippet.Title != "上游内容" {
			t.Fatalf("仍在使用旧解析数据：%+v", res.Snippet)
		}
	}
}

// 管理后台走的是 /internal/admin 下的这些路由。这里用真库把它们串一遍：
// 建账户 → 充值/核减 → 建接口 → 定价 → 汇总，顺带锁住后台依赖的 JSON 字段名。
func TestAdminRoutesEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pool := testPool(t)
	ctx := context.Background()

	cp := &chainParser{provider: uniqueProvider("admin")}
	adapterSlug := "t_admin_" + uuid.NewString()[:8]
	adapters := AdapterSet{adapterSlug: {
		Provider: cp.provider, Vendor: extapi.VendorRapidAPI, Note: "测试适配器",
		Request: func(Target) extapi.Request { return extapi.Request{URL: "http://unused"} },
		Decode:  func([]byte, Target) (*ParseResult, error) { return nil, nil },
	}}
	reg := NewRegistry()
	reg.Register(cp)
	ext := extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{}, adapters)
	svc := NewService(Deps{
		Registry: reg, Extapi: ext, Adapters: adapters,
		Prices: NewPriceRepo(pool), Logs: NewLogRepo(pool), DefaultPrice: 10,
	})

	r := gin.New()
	g := r.Group("/internal/admin")
	extapi.NewHandler(ext).RegisterInternal(g)
	NewHandler(svc).RegisterInternal(g)

	accSlug := "t_acc_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM parse_prices WHERE provider = $1`, cp.provider)
		_, _ = pool.Exec(ctx, `DELETE FROM api_endpoints WHERE provider = $1`, cp.provider)
		_, _ = pool.Exec(ctx, `DELETE FROM api_accounts WHERE slug = $1`, accSlug)
	})

	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, "/internal/admin"+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	find := func(items any, key, val string) map[string]any {
		t.Helper()
		list, _ := items.([]any)
		for _, it := range list {
			m := it.(map[string]any)
			if m[key] == val {
				return m
			}
		}
		t.Fatalf("列表里找不到 %s=%s", key, val)
		return nil
	}

	// 账户
	if code, _ := call("POST", "/api-accounts", map[string]any{"vendor": "nope", "slug": accSlug, "name": "x"}); code != 400 {
		t.Fatalf("未登记的上游应 400，得到 %d", code)
	}
	code, acc := call("POST", "/api-accounts", map[string]any{"vendor": "rapidapi", "slug": accSlug, "name": "RapidAPI · 测试"})
	if code != 200 {
		t.Fatalf("建账户失败 %d %v", code, acc)
	}
	accID := acc["id"].(string)
	if code, _ := call("POST", "/api-accounts", map[string]any{"vendor": "rapidapi", "slug": accSlug, "name": "dup"}); code != 409 {
		t.Fatalf("重复 slug 应 409，得到 %d", code)
	}

	// 充值 $5，核减 $0.5
	if code, _ := call("POST", "/api-accounts/"+accID+"/topup", map[string]any{"amount_micros": 5_000_000, "note": "支付宝"}); code != 200 {
		t.Fatalf("充值失败 %d", code)
	}
	if code, _ := call("POST", "/api-accounts/"+accID+"/topup", map[string]any{"amount_micros": -500_000, "note": "对账差额"}); code != 200 {
		t.Fatalf("核减失败 %d", code)
	}
	_, list := call("GET", "/api-accounts", nil)
	a := find(list["items"], "id", accID)
	if a["balance_micros"] != 4_500_000.0 || a["topped_up_micros"] != 5_000_000.0 || a["spent_micros"] != 500_000.0 {
		t.Errorf("账户汇总不对: %v", a)
	}
	if vendors, _ := list["vendors"].([]any); len(vendors) != 2 {
		t.Errorf("应列出两个上游: %v", list["vendors"])
	}
	_, ledger := call("GET", "/api-accounts/"+accID+"/ledger", nil)
	if items, _ := ledger["items"].([]any); len(items) != 2 || items[0].(map[string]any)["reason"] != "adjust" {
		t.Errorf("流水应为两笔、最新的是核减: %v", ledger["items"])
	}

	// 接口
	_, ad := call("GET", "/api-adapters", nil)
	find(ad["items"], "slug", adapterSlug)

	if code, _ := call("POST", "/api-endpoints", map[string]any{"slug": "no_such_adapter", "account_id": accID}); code != 400 {
		t.Fatalf("没有适配器的 slug 应 400，得到 %d", code)
	}
	var tikhubID string
	if err := pool.QueryRow(ctx, `SELECT id FROM api_accounts WHERE slug = 'tikhub'`).Scan(&tikhubID); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("POST", "/api-endpoints", map[string]any{"slug": adapterSlug, "account_id": tikhubID}); code != 400 {
		t.Fatalf("RapidAPI 适配器挂到 TikHub 账户应 400，得到 %d", code)
	}
	code, ep := call("POST", "/api-endpoints", map[string]any{"slug": adapterSlug, "account_id": accID, "unit_price_micros": 1_000})
	if code != 200 || ep["provider"] != cp.provider || ep["account_name"] != "RapidAPI · 测试" {
		t.Fatalf("建接口失败或平台没取自适配器: %d %v", code, ep)
	}
	epID := ep["id"].(string)
	if code, _ := call("PATCH", "/api-endpoints/"+epID, map[string]any{"unit_price_micros": 3_000}); code != 200 {
		t.Fatalf("改单价失败 %d", code)
	}
	_, got := call("GET", "/api-endpoints/"+epID, nil)
	if got["unit_price_micros"] != 3_000.0 || got["balance_micros"] != 4_500_000.0 {
		t.Errorf("接口详情不对: %v", got)
	}

	// 定价：自动价 = $0.003 × 2 = 60 代币
	_, prices := call("GET", "/parse-prices", nil)
	p := find(prices["items"], "provider", cp.provider)
	if p["auto_credits"] != 60.0 || p["effective_credits"] != 60.0 || p["override_credits"] != nil || p["endpoints"] != 1.0 {
		t.Errorf("定价不对: %v", p)
	}
	if code, _ := call("PUT", "/parse-prices/"+cp.provider, map[string]any{"credits": 0}); code != 400 {
		t.Errorf("0 代币应 400，得到 %d", code)
	}
	if code, _ := call("PUT", "/parse-prices/no_such_platform", map[string]any{"credits": 5}); code != 404 {
		t.Errorf("未知平台应 404，得到 %d", code)
	}
	if code, _ := call("PUT", "/parse-prices/"+cp.provider, map[string]any{"credits": 80}); code != 200 {
		t.Fatalf("设定售价失败 %d", code)
	}
	if code, _ := call("DELETE", "/parse-prices/"+cp.provider, nil); code != 200 {
		t.Fatalf("恢复自动价失败 %d", code)
	}

	// 汇总与列表的形状
	if code, sum := call("GET", "/parse-logs/summary?days=7", nil); code != 200 || sum["days"] != 7.0 {
		t.Errorf("汇总失败 %d %v", code, sum)
	}
	if code, _ := call("GET", "/parse-logs?before=2026-01-01T00:00:00Z", nil); code != 400 {
		t.Errorf("只给 before 不给 before_id 应 400，得到 %d", code)
	}
	if code, _ := call("GET", "/parse-logs?provider="+cp.provider, nil); code != 200 {
		t.Errorf("列表失败 %d", code)
	}
	if code, _ := call("POST", "/api-samples/"+uuid.NewString()+"/viewed", nil); code != 404 {
		t.Errorf("标记不存在的采样应 404，得到 %d", code)
	}
}
