package parser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 第二批的真实样本（2026-10-16 实测）。
func TestTikHubBatch2DecodeRealSamples(t *testing.T) {
	cases := []struct {
		slug, fixture string
		target        Target
		want          sampleWant
	}{
		{"tikhub_zhihu_web_fetch_answer_detail", "", target("answer/2054145988235880002", ""),
			sampleWant{kind: "article", urlPrefix: "https://www.zhihu.com/question/5721660615/answer/2054145988235880002", noAssets: true}},
		{"tikhub_zhihu_web_fetch_column_article_detail", "", target("p/669214677", ""),
			sampleWant{kind: "article", urlPrefix: "https://zhuanlan.zhihu.com/p/669214677", minImages: 1}},
		{"tikhub_zhihu_web_fetch_pin_detail", "", target("pin/2050290260991534603", ""),
			sampleWant{kind: "image", urlPrefix: "https://www.zhihu.com/pin/2050290260991534603", minImages: 10}},
		{"tikhub_zhihu_web_fetch_question_detail", "", target("question/37811449", ""),
			sampleWant{kind: "text", urlPrefix: "https://www.zhihu.com/question/37811449", noAuthor: true, noAssets: true}},
		{"tikhub_threads_web_fetch_post_detail_v2", "", target("DPVUglOjOUu", ""),
			sampleWant{kind: "image", urlPrefix: "https://www.threads.com/@taylorswift/post/DPVUglOjOUu", minImages: 4}},
		{"tikhub_threads_web_fetch_post_detail", "", target("C56JDdzi0RJ", ""),
			sampleWant{kind: "video", urlPrefix: "https://www.threads.com/@aiatmeta/post/C56JDdzi0RJ", video: true}},
		{"tikhub_reddit_app_fetch_post_details", "", target("90bu6w", ""),
			sampleWant{kind: "video", urlPrefix: "https://www.reddit.com/r/aww/comments/90bu6w/", video: true}},
		{"tikhub_reddit_app_fetch_post_details", "reddit_image", target("haucpf", ""),
			sampleWant{kind: "image", urlPrefix: "https://www.reddit.com/r/pics/comments/haucpf/", minImages: 1}},
		{"tikhub_reddit_app_fetch_post_details", "reddit_text", target("1x6oat7", ""),
			sampleWant{kind: "text", urlPrefix: "https://www.reddit.com/r/tifu/comments/1x6oat7/", noAssets: true}},
		// 视频文件加密，只有封面
		{"tikhub_wechat_channels_v2_fetch_video_detail", "", target("AH3sCoIPhH", "https://weixin.qq.com/sph/AH3sCoIPhH"),
			sampleWant{kind: "video", urlPrefix: "https://weixin.qq.com/sph/AH3sCoIPhH"}},
		{"tikhub_toutiao_web_get_article_info", "", target("7450114952884503059", "https://www.toutiao.com/article/7450114952884503059/"),
			sampleWant{kind: "article", urlPrefix: "https://www.toutiao.com/article/7450114952884503059/", minImages: 1}},
		{"tikhub_toutiao_web_get_article_info", "toutiao_weitoutiao", target("1878680639539276", "https://www.toutiao.com/w/1878680639539276/"),
			sampleWant{kind: "image", urlPrefix: "https://www.toutiao.com/w/1878680639539276/", minImages: 15}},
		{"tikhub_toutiao_web_get_video_info", "", target("7695721913519177756", "https://www.toutiao.com/video/7695721913519177756/"),
			sampleWant{kind: "video", urlPrefix: "https://www.toutiao.com/video/7695721913519177756/", video: true}},
		{"tikhub_toutiao_web_get_video_info", "toutiao_web_video_xigua", target("7354954305222377999", "https://www.ixigua.com/7354954305222377999"),
			sampleWant{kind: "video", urlPrefix: "https://www.toutiao.com/video/7354954305222377999/", video: true}},
		{"tikhub_toutiao_app_get_video_info", "", target("7354954305222377999", "https://www.ixigua.com/7354954305222377999"),
			sampleWant{kind: "video", urlPrefix: "https://www.toutiao.com/video/7354954305222377999/", video: true}},
		// App 版的短视频形态：作者、计数、播放地址都换了位置
		{"tikhub_toutiao_app_get_video_info", "toutiao_app_short_video", target("7695721913519177756", ""),
			sampleWant{kind: "video", urlPrefix: "https://www.toutiao.com/video/7695721913519177756/", video: true}},
		{"tikhub_lemon8_app_fetch_post_detail", "", target("7445613324903006766", ""),
			sampleWant{kind: "video", urlPrefix: "https://www.lemon8-app.com/@deathlabs_/7445613324903006766", video: true}},
		{"tikhub_lemon8_app_fetch_post_detail", "lemon8_gallery", target("7586839908082319902", ""),
			sampleWant{kind: "image", urlPrefix: "https://www.lemon8-app.com/@soe98632/7586839908082319902", minImages: 5}},
		{"tikhub_pipixia_app_fetch_post_detail", "", target("7411193113223371043", ""),
			sampleWant{kind: "video", urlPrefix: "https://h5.pipix.com/item/7411193113223371043", video: true}},
		{"tikhub_linkedin_web_v2_get_post_detail", "", target("activity/7434688896669560833",
			"https://www.linkedin.com/posts/satyanadella_counting-down-to-a-new-build-in-san-francisco-activity-7434688896669560833-PqKW?utm_source=share"),
			sampleWant{kind: "video", urlPrefix: "https://www.linkedin.com/posts/satyanadella_counting-down", video: true}},
		{"tikhub_telegram_web_fetch_post_detail", "", target("durov/510", ""),
			sampleWant{kind: "image", urlPrefix: "https://t.me/durov/510", minImages: 2}},
		{"tikhub_telegram_web_fetch_post_detail", "telegram_video", target("durov/546", ""),
			sampleWant{kind: "video", urlPrefix: "https://t.me/durov/546", video: true}},
		// 大视频网页预览不给文件：仍是视频作品，只有封面
		{"tikhub_telegram_web_fetch_post_detail", "telegram_video_too_big", target("durov/552", ""),
			sampleWant{kind: "video", urlPrefix: "https://t.me/durov/552"}},
	}
	for _, c := range cases {
		name := c.fixture
		if name == "" {
			name = strings.TrimPrefix(c.slug, "tikhub_")
		}
		t.Run(name, func(t *testing.T) {
			res, p := decodeWith(t, c.slug, name, c.target)
			checkSample(t, res, p, c.want)
		})
	}
}

// 各平台的细节：最高清晰度、标题取哪个字段、正文怎么来。
func TestTikHubBatch2Details(t *testing.T) {
	// 知乎回答：标题是所属问题，正文去掉 HTML、按段落换行
	res, ans := decodeWith(t, "tikhub_zhihu_web_fetch_answer_detail", "zhihu_web_fetch_answer_detail", target("answer/2054145988235880002", ""))
	if ans.Title != "有没有一上来就很刺激的甜文！?" || strings.Contains(ans.Text, "<p") || !strings.Contains(ans.Text, "\n") {
		t.Errorf("知乎回答: title=%q text=%q", ans.Title, truncateRunes(ans.Text, 60))
	}
	if strings.Contains(string(res.Payload), `"stats"`) {
		t.Error("一个计数都没有时不写 stats")
	}
	// 专栏文章取 data-original（_r 原图），不取 1440w 压缩图
	_, art := decodeWith(t, "tikhub_zhihu_web_fetch_column_article_detail", "zhihu_web_fetch_column_article_detail", target("p/669214677", ""))
	if len(art.Images) != 1 || !strings.HasSuffix(art.Images[0].URL, "_r.jpg") || art.Images[0].Width != 1028 {
		t.Errorf("知乎文章图片: %+v", art.Images)
	}
	// 想法取 original_url（无水印原图）
	_, pin := decodeWith(t, "tikhub_zhihu_web_fetch_pin_detail", "zhihu_web_fetch_pin_detail", target("pin/2050290260991534603", ""))
	if strings.Contains(pin.Images[0].URL, "_720w") || pin.Images[0].Width != 1917 {
		t.Errorf("知乎想法应取原图: %+v", pin.Images[0])
	}

	// 微头条没有标题（h5_extra.title 只是正文开头），正文来自 <article> 里的 JSON
	wres, wtt := decodeWith(t, "tikhub_toutiao_web_get_article_info", "toutiao_weitoutiao", target("1878680639539276", ""))
	if wtt.Title != "" || wres.Description == nil || strings.HasPrefix(*wres.Description, "{") {
		t.Errorf("微头条: title=%q desc=%v", wtt.Title, wres.Description)
	}
	// 头条网页版视频：标题去掉分享后缀，1080p
	_, ttv := decodeWith(t, "tikhub_toutiao_web_get_video_info", "toutiao_web_get_video_info", target("7695721913519177756", ""))
	if strings.Contains(ttv.Title, "今日头条") || ttv.Video == nil || ttv.Video.Height != 1920 {
		t.Errorf("头条网页版视频: title=%q video=%+v", ttv.Title, ttv.Video)
	}
	// App 版长视频：video_meta 里取 1080p（H.264 3 Mbps）；短视频：按清晰度名取 1080p，尺寸按比例换算
	_, tta := decodeWith(t, "tikhub_toutiao_app_get_video_info", "toutiao_app_get_video_info", target("7354954305222377999", ""))
	if tta.Video == nil || tta.Video.Width != 1080 || tta.Video.Height != 1440 {
		t.Errorf("头条 App 版长视频: %+v", tta.Video)
	}
	_, tts := decodeWith(t, "tikhub_toutiao_app_get_video_info", "toutiao_app_short_video", target("7695721913519177756", ""))
	if tts.Video == nil || tts.Video.Width != 1080 || tts.Video.Height != 1920 || tts.Author == nil || tts.Author.Name != "足坛宙斯" {
		t.Errorf("头条 App 版短视频: video=%+v author=%+v", tts.Video, tts.Author)
	}

	// Lemon8 视频取 1080p；图文的无水印图只有 HEIC，退到 webp
	_, l8v := decodeWith(t, "tikhub_lemon8_app_fetch_post_detail", "lemon8_app_fetch_post_detail", target("7445613324903006766", ""))
	if l8v.Video == nil || l8v.Video.Height != 1920 || isHEIC(l8v.Cover) || l8v.Text != "" {
		t.Errorf("Lemon8 视频: video=%+v cover=%s text=%q", l8v.Video, l8v.Cover, l8v.Text)
	}
	_, l8g := decodeWith(t, "tikhub_lemon8_app_fetch_post_detail", "lemon8_gallery", target("7586839908082319902", ""))
	for _, im := range l8g.Images {
		if isHEIC(im.URL) {
			t.Errorf("Lemon8 不该给 HEIC: %s", im.URL)
		}
	}

	// 皮皮虾取原片（同分辨率码率最高的那档）
	_, ppx := decodeWith(t, "tikhub_pipixia_app_fetch_post_detail", "pipixia_app_fetch_post_detail", target("7411193113223371043", ""))
	if ppx.Video == nil || !strings.Contains(ppx.Video.URL, "br=1474") {
		t.Errorf("皮皮虾应取原片: %+v", ppx.Video)
	}

	// Reddit 视频取带音轨的合成 mp4 最高档；图片帖取 i.redd.it 原图
	_, rv := decodeWith(t, "tikhub_reddit_app_fetch_post_details", "reddit_app_fetch_post_details", target("90bu6w", ""))
	if rv.Video == nil || !strings.Contains(rv.Video.URL, "/muxed.mp4") || rv.Video.Height != 1080 {
		t.Errorf("Reddit 视频: %+v", rv.Video)
	}
	rres, _ := decodeWith(t, "tikhub_reddit_app_fetch_post_details", "reddit_text", target("1x6oat7", ""))
	if rres.Description == nil || !strings.Contains(*rres.Description, "TLDR") {
		t.Errorf("Reddit 文字帖正文: %v", rres.Description)
	}

	// Threads v1：视频版本不带尺寸，取帖子原始尺寸
	_, th := decodeWith(t, "tikhub_threads_web_fetch_post_detail", "threads_web_fetch_post_detail", target("C56JDdzi0RJ", ""))
	if th.Video == nil || th.Video.Width != 1920 {
		t.Errorf("Threads 视频: %+v", th.Video)
	}

	// Telegram 的缩写计数
	_, tg := decodeWith(t, "tikhub_telegram_web_fetch_post_detail", "telegram_web_fetch_post_detail", target("durov/510", ""))
	if tg.Stats == nil || tg.Stats.Views == nil || *tg.Stats.Views != 3830000 {
		t.Errorf("Telegram 浏览数: %+v", tg.Stats)
	}
}

// 失败形态与对不上的作品都必须当失败，让兜底链继续。
func TestTikHubBatch2Rejects(t *testing.T) {
	a := TikHubAdapters()
	cases := []struct {
		name, slug, fixture string
		target              Target
		want                error // nil 表示只要失败就行
	}{
		{"知乎回答 ID 对不上", "tikhub_zhihu_web_fetch_answer_detail", "zhihu_web_fetch_answer_detail", target("answer/1", ""), errWrongItem},
		{"Threads shortcode 对不上", "tikhub_threads_web_fetch_post_detail_v2", "threads_web_fetch_post_detail_v2", target("DOTHER12345", ""), errWrongItem},
		{"Reddit ID 对不上", "tikhub_reddit_app_fetch_post_details", "reddit_image", target("abc", ""), errWrongItem},
		{"头条 ID 对不上", "tikhub_toutiao_web_get_video_info", "toutiao_web_get_video_info", target("1", ""), errWrongItem},
		{"Telegram 频道对不上", "tikhub_telegram_web_fetch_post_detail", "telegram_web_fetch_post_detail", target("other/510", ""), errWrongItem},
		{"LinkedIn activity 对不上", "tikhub_linkedin_web_v2_get_post_detail", "linkedin_web_v2_get_post_detail", target("activity/1", ""), errWrongItem},
		// group 链接看不出类型时先试文章接口：是视频就交给视频接口
		{"头条文章接口拿到视频", "tikhub_toutiao_web_get_article_info", "toutiao_article_on_video", target("7695721913519177756", ""), errOtherKind},
		// HTTP 200（计费）但其实失败
		{"Lemon8 InternalErr", "tikhub_lemon8_app_fetch_post_detail", "lemon8_internal_error", target("7361926875709129222", ""), nil},
		{"LinkedIn 超时", "tikhub_linkedin_web_v2_get_post_detail", "linkedin_timeout", target("activity/7369432203736305666", ""), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := a[c.slug].Decode(fixture(t, c.fixture), c.target)
			if err == nil {
				t.Fatal("应当失败")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("应是 %v，得到 %v", c.want, err)
			}
		})
	}
}

// 按链接分流：知乎按 ID 前缀，头条按链接类型，Threads v1 只接换得出数字 ID 的 shortcode。
func TestTikHubBatch2Accept(t *testing.T) {
	a := TikHubAdapters()
	for slug, own := range map[string]string{
		"tikhub_zhihu_web_fetch_answer_detail":         "answer/1",
		"tikhub_zhihu_web_fetch_column_article_detail": "p/1",
		"tikhub_zhihu_web_fetch_pin_detail":            "pin/1",
		"tikhub_zhihu_web_fetch_question_detail":       "question/1",
	} {
		for _, id := range []string{"answer/1", "p/1", "pin/1", "question/1"} {
			if got := a[slug].Accept(target(id, "")); got != (id == own) {
				t.Errorf("%s 接 %s = %v", slug, id, got)
			}
		}
	}
	article, webVideo, appVideo := a["tikhub_toutiao_web_get_article_info"].Accept,
		a["tikhub_toutiao_web_get_video_info"].Accept, a["tikhub_toutiao_app_get_video_info"].Accept
	for resolved, want := range map[string][3]bool{ // 文章 / 网页视频 / App 视频
		"https://www.toutiao.com/article/7450114952884503059/": {true, false, false},
		"https://www.toutiao.com/w/1878680639539276/":          {true, false, false},
		"https://m.toutiao.com/video/7695721913519177756/":     {false, true, true},
		"https://www.ixigua.com/7354954305222377999":           {false, true, true},
		"https://www.toutiao.com/group/7354954305222377999/":   {true, true, true}, // 看不出类型
	} {
		tg := target("1", resolved)
		if got := [3]bool{article(tg), webVideo(tg), appVideo(tg)}; got != want {
			t.Errorf("%s 分流 %v，期望 %v", resolved, got, want)
		}
	}
	if !a["tikhub_threads_web_fetch_post_detail"].Accept(target("C56JDdzi0RJ", "")) ||
		a["tikhub_threads_web_fetch_post_detail"].Accept(target("CsomethingVeryLongPrivateCode", "")) {
		t.Error("Threads v1 只接换得出数字 ID 的 shortcode")
	}
	// v1 请求的是 shortcode 换算出的数字 ID（实测样本 pk=3349029093483693129）
	if q := a["tikhub_threads_web_fetch_post_detail"].Request(target("C56JDdzi0RJ", "")).Query["post_id"]; q != "3349029093483693129" {
		t.Errorf("Threads v1 post_id=%s", q)
	}
	if q := a["tikhub_reddit_app_fetch_post_details"].Request(target("90bu6w", "")).Query["post_id"]; q != "t3_90bu6w" {
		t.Errorf("Reddit post_id=%s", q)
	}
	if q := a["tikhub_telegram_web_fetch_post_detail"].Request(target("durov/510", "")).Query; q["channel"] != "durov" || q["post_id"] != "510" {
		t.Errorf("Telegram 参数 %v", q)
	}
}

func TestBatch2PlatformMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewRedditProvider())
	for _, p := range NewUpstreamPlatforms() {
		reg.Register(p)
	}
	cases := []struct{ uri, provider, id string }{
		{"https://www.zhihu.com/question/5721660615/answer/2054145988235880002?share_code=x", PlatformZhihu, "answer/2054145988235880002"},
		{"https://www.zhihu.com/answer/2054145988235880002", PlatformZhihu, "answer/2054145988235880002"},
		{"https://zhuanlan.zhihu.com/p/669214677", PlatformZhihu, "p/669214677"},
		{"https://www.zhihu.com/pin/2050290260991534603", PlatformZhihu, "pin/2050290260991534603"},
		{"https://www.zhihu.com/question/37811449", PlatformZhihu, "question/37811449"},
		{"https://www.threads.com/@taylorswift/post/DPVUglOjOUu?xmt=x", PlatformThreads, "DPVUglOjOUu"},
		{"https://www.threads.net/@aiatmeta/post/C56JDdzi0RJ", PlatformThreads, "C56JDdzi0RJ"},
		{"https://www.reddit.com/r/aww/comments/90bu6w/heat_index/", PlatformReddit, "90bu6w"},
		{"https://weixin.qq.com/sph/AH3sCoIPhH", PlatformWechatChannels, "AH3sCoIPhH"},
		{"https://channels.weixin.qq.com/finder-preview/pages/sph?id=AH3sCoIPhH", PlatformWechatChannels, "AH3sCoIPhH"},
		{"https://www.toutiao.com/article/7450114952884503059/?log_from=x", PlatformToutiao, "7450114952884503059"},
		{"https://m.toutiao.com/video/7695721913519177756/", PlatformToutiao, "7695721913519177756"},
		{"https://www.toutiao.com/w/1878680639539276/", PlatformToutiao, "1878680639539276"},
		{"https://www.toutiao.com/group/7354954305222377999/", PlatformToutiao, "7354954305222377999"},
		{"https://www.ixigua.com/7354954305222377999?logTag=x", PlatformToutiao, "7354954305222377999"},
		{"https://www.lemon8-app.com/@deathlabs_/7445613324903006766?region=us", PlatformLemon8, "7445613324903006766"},
		{"https://h5.pipix.com/item/7411193113223371043", PlatformPipixia, "7411193113223371043"},
		{"https://h5.pipix.com/ppx/item/7411193113223371043?app_id=1319", PlatformPipixia, "7411193113223371043"},
		{"https://www.linkedin.com/posts/satyanadella_counting-down-activity-7434688896669560833-PqKW", PlatformLinkedIn, "activity/7434688896669560833"},
		{"https://www.linkedin.com/feed/update/urn:li:activity:7434688896669560833/", PlatformLinkedIn, "activity/7434688896669560833"},
		{"https://www.linkedin.com/posts/someone_title-ugcPost-7300000000000000000-AbCd", PlatformLinkedIn, "ugcPost/7300000000000000000"},
		{"https://www.linkedin.com/pulse/how-we-build-satya-nadella", PlatformLinkedIn, "pulse/how-we-build-satya-nadella"},
		{"https://t.me/durov/510", PlatformTelegram, "durov/510"},
		{"https://t.me/s/Durov/510", PlatformTelegram, "durov/510"},
	}
	for _, c := range cases {
		p, id, ok := reg.Find(c.uri)
		if !ok || p.Provider() != c.provider || id != c.id {
			got := ""
			if ok {
				got = p.Provider()
			}
			t.Errorf("%s → (%s, %s, %v)，期望 (%s, %s)", c.uri, got, id, ok, c.provider, c.id)
		}
	}
	// Telegram 的个人 / 频道主页、私有频道、加群链接都不是公开消息
	for _, uri := range []string{"https://t.me/durov", "https://t.me/c/1234567890/45", "https://t.me/joinchat/AbCdEf",
		"https://www.zhihu.com/people/zhang-jia-wei", "https://www.lemon8-app.com/@deathlabs_"} {
		if p, _, ok := reg.Find(uri); ok {
			t.Errorf("%s 不该被 %s 认领", uri, p.Provider())
		}
	}
	for uri, want := range map[string]string{
		"https://m.toutiao.com/is/iRQx7Ftb/": PlatformToutiao,
		"https://v.ixigua.com/iRQx7Ftb/":     PlatformToutiao,
		"https://h5.pipix.com/s/iRQx7Ftb/":   PlatformPipixia,
	} {
		if p, ok := reg.FindShortLink(uri); !ok || p.Provider() != want {
			t.Errorf("短链 %s 应归 %s", uri, want)
		}
	}
}

// 内置免费来源与付费接口产出同一种 post 形状。
func TestRedditBuiltinUsesPostShape(t *testing.T) {
	body := `[{"data":{"children":[{"data":{"title":"A title","author":"someone","url":"https://example.com/story",
		"domain":"example.com","permalink":"/r/test/comments/abc123/a_title/","selftext":"Body text",
		"thumbnail":"https://b.thumbs.redditmedia.com/x.jpg","created_utc":1729000000.0,"score":42,"num_comments":7}}]}}]`
	p := &RedditProvider{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/comments/abc123.json" {
			t.Errorf("请求了 %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	res, err := p.Parse(context.Background(), "https://redd.it/abc123", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	var got decodedPost
	_ = json.Unmarshal(res.Payload, &got)
	if got.URL != "https://www.reddit.com/r/test/comments/abc123/a_title/" || got.Kind != "text" || got.Title != "A title" ||
		got.Author == nil || got.Author.Handle != "u/someone" || got.Stats == nil || *got.Stats.Likes != 42 {
		t.Errorf("payload 不是 post 形状: %s", res.Payload)
	}
	if !strings.Contains(*res.Description, "https://example.com/story") {
		t.Errorf("链接帖的外链应写进正文: %q", *res.Description)
	}
	if len(res.Assets) != 1 || res.Assets[0].Role != "thumbnail" {
		t.Errorf("缩略图: %+v", res.Assets)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
