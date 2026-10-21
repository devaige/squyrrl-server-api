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

	"github.com/gin-gonic/gin/binding"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/wallet"
)

// 用户实际复制到的抖音文章口令（2026-10-21）。码两边的符号每次不同。
var douyinCodes = map[string]string{
	"0.:/ :6pm d@n.QX 08/29 【刚刚，官宣！要捐32亿分给普通】长按复制打开抖音，即可阅读文章 ➝➝tnlUWI2uYFa9※※":                  "tnlUWI2uYFa9",
	"8k A@T.Yz :9pm 10/17 【刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实:/ 测】长按复制打开抖音，即可阅读文章 ŠŠcz9zRIbcJa9ŠŠ\n": "cz9zRIbcJa9",
	"8:/ h@O.kP 05/21 【生意场上，装逼就是节省沟通成本】长按复制打开抖音，即可阅读文章 ︽︽2QnCB9aIZZ29︽︽":                        "2QnCB9aIZZ29",
}

func TestDouyinShareCodeMatch(t *testing.T) {
	reg := NewRegistry()
	for _, p := range NewUpstreamPlatforms() {
		reg.Register(p)
	}
	for text, want := range douyinCodes {
		p, code, ok := reg.FindShareCode(text)
		if !ok || p.Provider() != PlatformDouyin || code != want {
			t.Errorf("%q：认成 %v / %q，期望抖音 %q", text, ok, code, want)
		}
	}
	for _, text := range []string{
		// 视频口令带链接，由客户端取链接；没有「文章」的口令（主页之类）没有内容可存
		"7.43 复制打开抖音，看看【某某的作品】好看 https://v.douyin.com/AbCdEf/ a@B.aB 05/12",
		"长按复制此条消息，打开抖音搜索，查看TA的更多作品。 ︽︽abcdEFGH12︽︽",
		// 用户自己写的笔记
		"今天读了一篇长按复制打开抖音的文章，讲 Anthropic 的事",
		"长按复制打开抖音，即可阅读文章 ︽︽tnlUWI2uYFa9︽︽ 我觉得写得不错",
	} {
		if _, code, ok := reg.FindShareCode(text); ok {
			t.Errorf("%q 不该认成口令（码 %q）", text, code)
		}
	}
}

// 送给换口令接口的文字：只把码两边换成 ︽︽，其余照原样（用户实测：ŠŠ 失败，换成 ︽︽ 成功）。
func TestDouyinShareText(t *testing.T) {
	cases := []struct{ in, code, want string }{
		{"8k A@T.Yz :9pm 10/17 【刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实:/ 测】长按复制打开抖音，即可阅读文章 ŠŠcz9zRIbcJa9ŠŠ", "cz9zRIbcJa9",
			"8k A@T.Yz :9pm 10/17 【刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实:/ 测】长按复制打开抖音，即可阅读文章 ︽︽cz9zRIbcJa9︽︽"},
		{"…即可阅读文章 ➝➝tnlUWI2uYFa9※※\n", "tnlUWI2uYFa9", "…即可阅读文章 ︽︽tnlUWI2uYFa9︽︽"},
		{"…即可阅读文章︽︽2QnCB9aIZZ29︽︽", "2QnCB9aIZZ29", "…即可阅读文章︽︽2QnCB9aIZZ29︽︽"},
	}
	for _, c := range cases {
		if got := douyinShareText(c.in, c.code); got != c.want {
			t.Errorf("douyinShareText(%q)\n得到 %q\n期望 %q", c.in, got, c.want)
		}
	}
}

func TestDouyinShareInfo(t *testing.T) {
	ad := TikHubAdapters()["tikhub_douyin_app_v3_fetch_share_info_by_share_code"]
	link, err := ad.Resolve(fixture(t, "douyin_app_v3_fetch_share_info_by_share_code"), Target{})
	if err != nil || link != "https://www.iesdouyin.com/share/video/7698925399581804456/" {
		t.Fatalf("换出 %q %v", link, err)
	}
	var douyin Parser
	for _, p := range NewUpstreamPlatforms() {
		if p.Provider() == PlatformDouyin {
			douyin = p
		}
	}
	if id, ok := douyin.Match(link); !ok || id != "7698925399581804456" {
		t.Errorf("换来的链接抖音要认得：%q %v", id, ok)
	}
	if _, err := ad.Resolve([]byte(`{"status_code":0,"share_scene":"user","extra_info":{"object_id":"123"}}`), Target{}); err == nil {
		t.Error("不是文章的口令不该换出作品链接")
	}
	req := ad.Request(Target{URI: "…阅读文章 ŠŠcz9zRIbcJa9ŠŠ", Code: "cz9zRIbcJa9"})
	if req.Query["share_code"] != "…阅读文章 ︽︽cz9zRIbcJa9︽︽" {
		t.Errorf("请求里的口令：%q", req.Query["share_code"])
	}
	if pf, ok := douyin.(PriceFactorer); !ok || pf.PriceFactor(Target{Code: "x"}) != 2 || pf.PriceFactor(Target{}) != 1 {
		t.Error("口令按 2 倍、链接按原价")
	}
}

// 抖音文章：标题、markdown 转出的纯文本、按正文顺序的原图档插图，没有视频（play_addr 是配乐）。
func TestDouyinArticle(t *testing.T) {
	id := "7698925399581804456"
	res, p := decodeWith(t, "tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_article", target(id, ""))
	checkSample(t, res, p, sampleWant{kind: "article", urlPrefix: "https://www.iesdouyin.com/share/video/" + id + "/", minImages: 3})

	if p.Title != "刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实测" {
		t.Errorf("标题 %q", p.Title)
	}
	if !strings.HasPrefix(p.Text, "刚刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实测\nClaude最新成果") ||
		!strings.HasSuffix(p.Text, "如今战场又换到了生命科学。") {
		t.Errorf("正文 %q", p.Text)
	}
	for _, junk := range []string{"![", "<br", "heic", "\n\n"} {
		if strings.Contains(p.Text, junk) {
			t.Errorf("正文里不该有 %q", junk)
		}
	}
	if p.Video != nil {
		t.Errorf("文章没有视频: %+v", p.Video)
	}
	if len(p.Images) != 3 {
		t.Fatalf("插图 %d 张", len(p.Images))
	}
	wantDims := [][2]int{{1080, 760}, {1080, 350}, {1080, 650}}
	for i, img := range p.Images {
		if !strings.Contains(img.URL, "~tplv-shrink:0:0.heic") {
			t.Errorf("第 %d 张应换成原图档: %s", i+1, img.URL)
		}
		if img.Width != wantDims[i][0] || img.Height != wantDims[i][1] {
			t.Errorf("第 %d 张尺寸 %dx%d", i+1, img.Width, img.Height)
		}
	}
	if !strings.Contains(p.Images[0].URL, "oAJL1LfDIQGIAfYiEweeAA1A2A2VrGcATAm7Ky") ||
		!strings.Contains(p.Images[2].URL, "oI9AE2anCIABaAK7RVEPYfAoiwElfCDDqEA9AF") {
		t.Error("插图顺序要跟正文一致")
	}
	if res.Assets[0].Role != "thumbnail" || !strings.Contains(res.Assets[0].URL, ".webp") {
		t.Errorf("封面应是能解的 webp：%+v", res.Assets[0])
	}
	// 全文留在 payload 里（文章即使不长也留，见 post.result）
	if !strings.Contains(string(res.Payload), `"text":"刚刚`) {
		t.Error("payload 里要有全文")
	}

	// 是文章却没有正文：解码失败，兜底链换下一个接口，而不是存一段配乐
	var raw map[string]map[string]any
	if err := json.Unmarshal(fixture(t, "douyin_app_v3_article"), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw["aweme_detail"], "article_info")
	data, _ := json.Marshal(raw)
	if _, err := TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"].Decode(data, target(id, "")); err == nil {
		t.Error("aweme_type 163 而没有 article_info 应当失败")
	}
}

func TestMarkdownText(t *testing.T) {
	md := "## 小标题\n\n**加粗**的一段\n\n![](https://x.test/a~t.heic width=10 height=20)\n\n第二段<br/>\n\n<br/>\n"
	if got := markdownText(md); got != "小标题\n加粗的一段\n第二段" {
		t.Errorf("%q", got)
	}
}

// 口令与链接二选一；链接得是链接。
func TestParseInputBinding(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"uri":"https://www.douyin.com/video/1"}`: true,
		`{"share_code":"…阅读文章 ︽︽abcdEFGH12︽︽"}`:    true,
		`{}`:                         false,
		`{"uri":"not a link"}`:       false,
		`{"force":true}`:             false,
		`{"uri":"","share_code":""}`: false,
	} {
		var in ParseInput
		err := binding.JSON.BindBody([]byte(body), &in)
		if (err == nil) != ok {
			t.Errorf("%s：err=%v，期望通过=%v", body, err, ok)
		}
	}
}

// 清单里的口令规则只在平台有能用的换口令接口时下发；它不抬售价、不算取内容的接口。
func TestShareCodeManifestNeedsResolver(t *testing.T) {
	plat := &platform{id: "codes", canonical: res(`codes\.test/item/(\w+)`), codes: res(`口令 (\w+)$`)}
	reg := NewRegistry()
	reg.Register(plat)
	if m := reg.Manifest(nil, func(Parser) bool { return false }); len(m.Providers) != 1 || m.Providers[0].Codes != nil {
		t.Errorf("没有换口令接口时不下发口令规则: %+v", m.Providers)
	}
	m := reg.Manifest(nil, nil)
	if len(m.Providers[0].Codes) != 1 || m.Providers[0].Codes[0] != `口令 (\w+)$` {
		t.Errorf("口令规则: %+v", m.Providers[0])
	}

	adapters := AdapterSet{
		"t_src": {Provider: "codes", Vendor: extapi.VendorTikHub},
		"t_res": {Provider: "codes", Vendor: extapi.VendorTikHub, Resolve: func([]byte, Target) (string, error) { return "", nil }},
	}
	eps := []extapi.Endpoint{
		{Slug: "t_src", Provider: "codes", Vendor: extapi.VendorTikHub, UnitPriceMicros: 1_000},
		{Slug: "t_res", Provider: "codes", Vendor: extapi.VendorTikHub, UnitPriceMicros: 5_000},
	}
	if got := adapters.maxUnitPrice(eps); got != 1_000 {
		t.Errorf("换口令接口不该抬售价：最高单价 %d", got)
	}
}

// ---- 真库：口令 → 换链接 → 兜底链 → 按 2 倍扣费，两本账对得上 ----

// fakeShareCodeUpstream：/share 把口令换成作品链接（口令里带 bad 时换不出），/item 返回正文。
func fakeShareCodeUpstream(t *testing.T, provider string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var shares atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/share":
			shares.Add(1)
			code := r.URL.Query().Get("share_code")
			if strings.Contains(code, "bad") {
				_, _ = w.Write([]byte(`{"code":200,"data":{"link":""}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"link":"https://` + provider + `.test/item/` + code + `"}}`))
		case "/item":
			_, _ = w.Write([]byte(`{"code":200,"data":{"desc":"文章正文"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &shares
}

func TestShareCodeParsesAndChargesDouble(t *testing.T) {
	provider := uniqueProvider("code")
	srv, shares := fakeShareCodeUpstream(t, provider)
	plat := &platform{id: provider,
		canonical: res(provider + `\.test/item/(\w+)`),
		codes:     res(`打开 (\w+)$`),
		priceFactor: func(t Target) int64 {
			if t.Code != "" {
				return 2
			}
			return 1
		},
	}
	resolver := Adapter{Provider: provider, Vendor: extapi.VendorTikHub,
		Request: func(t Target) extapi.Request {
			return extapi.Request{URL: srv.URL + "/share", Query: map[string]string{"share_code": t.Code}}
		},
		Resolve: func(data []byte, _ Target) (string, error) {
			var d struct {
				Link string `json:"link"`
			}
			if err := json.Unmarshal(data, &d); err != nil || d.Link == "" {
				return "", errors.New("没有链接")
			}
			return d.Link, nil
		},
	}
	adapters := AdapterSet{"t_code_res": resolver, "t_code_item": testAdapter(provider, srv.URL+"/item")}
	env := newChainEnv(t, plat, adapters, 1_000_000)
	env.addEndpoint(t, "t_code_res", 1_000)
	env.addEndpoint(t, "t_code_item", 1_000) // 自动价 20，口令 40
	svc := env.service(10)
	ctx := context.Background()

	m, err := svc.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Providers {
		if e.ID == provider && len(e.Codes) != 1 {
			t.Errorf("有换口令接口时要下发口令规则: %+v", e)
		}
	}

	uid := env.user(t, 1_000)
	res, err := svc.Parse(ctx, uid, ParseInput{ShareCode: "复制打开 abc123"})
	if err != nil {
		t.Fatalf("口令解析失败: %v", err)
	}
	if res.ResourceID != "abc123" || res.CreditsCharged != 40 || env.balance(t, uid) != 960 {
		t.Errorf("应按作品 ID 解析、收 2 倍 40：%+v，余额 %d", res, env.balance(t, uid))
	}
	log := env.lastLog(t)
	if len(log.Attempts) != 2 || log.Attempts[0].Source != "t_code_res" || log.Attempts[1].Source != "t_code_item" ||
		log.Attempts[1].Seq != 2 || log.CostMicros != 2_000 || log.ResourceID != "abc123" {
		t.Errorf("记录要先列换口令那次、成本两次都算: %+v", log)
	}

	// 同一作品再来一次口令：内容命中缓存，换口令那次照付照记
	res, err = svc.Parse(ctx, uid, ParseInput{ShareCode: "复制打开 abc123"})
	if err != nil || !res.Cached || res.CreditsCharged != 40 {
		t.Fatalf("缓存命中照样 2 倍：%+v %v", res, err)
	}
	if log := env.lastLog(t); !log.Cached || len(log.Attempts) != 1 || log.CostMicros != 1_000 {
		t.Errorf("缓存命中的记录只有换口令那一次: %+v", log)
	}
	// 用换来的作品链接刷新：原价，不再换口令
	before := shares.Load()
	if res, err := svc.Parse(ctx, uid, ParseInput{URI: "https://" + provider + ".test/item/abc123"}); err != nil || res.CreditsCharged != 20 {
		t.Errorf("作品链接按原价 20：%+v %v", res, err)
	}
	if shares.Load() != before {
		t.Error("作品链接不该再调换口令接口")
	}

	// 换不出来：不扣费，记一笔失败，成本照记
	balance := env.balance(t, uid)
	if _, err := svc.Parse(ctx, uid, ParseInput{ShareCode: "复制打开 bad1"}); !errors.Is(err, ErrParseFailed) {
		t.Fatalf("换不出口令应返回通用失败，得到 %v", err)
	}
	if env.balance(t, uid) != balance {
		t.Error("口令换不出来不应扣费")
	}
	if log := env.lastLog(t); log.Status != LogFailed || log.ResourceID != "口令:bad1" || log.CostMicros != 1_000 ||
		len(log.Attempts) != 1 || log.Attempts[0].ErrorKind != extapi.KindDecode {
		t.Errorf("应记一笔口令失败: %+v", log)
	}

	// 余额不够 2 倍：换口令之前就挡下，不花上游的钱
	poor := env.user(t, 30)
	calls := shares.Load()
	_, err = svc.Parse(ctx, poor, ParseInput{ShareCode: "复制打开 xyz789"})
	var ie *wallet.InsufficientCreditsError
	if !errors.As(err, &ie) || ie.Required != 40 || shares.Load() != calls {
		t.Errorf("余额预检应在换口令之前、按 2 倍：%v，换口令调用 %d→%d", err, calls, shares.Load())
	}

	// 认不出的口令、平台没有换口令接口：都当「没有解析器」，不收费
	if _, err := svc.Parse(ctx, uid, ParseInput{ShareCode: "随便一段话"}); !errors.Is(err, ErrNoParser) {
		t.Errorf("认不出的口令：%v", err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE api_endpoints SET enabled = FALSE WHERE provider = $1 AND slug = 't_code_res'`, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Parse(ctx, uid, ParseInput{ShareCode: "复制打开 def456"}); !errors.Is(err, ErrNoParser) {
		t.Errorf("没有换口令接口：%v", err)
	}
}
