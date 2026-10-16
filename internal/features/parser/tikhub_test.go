package parser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/squyrrl/api/internal/features/extapi"
)

// testdata/tikhub/ 下是 2026-10-16 实测的 TikHub 响应（只留 data，删掉了解码用不到的大字段）。
// 文件名 = 适配器 slug 去掉 tikhub_ 前缀；另有几份失败形态的样本。

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tikhub/" + name + ".json")
	if err != nil {
		t.Fatalf("读样本 %s: %v", name, err)
	}
	return b
}

type decodedPost struct {
	post
	Text string `json:"text"`
}

func decodeWith(t *testing.T, slug, fixtureName string, tg Target) (*ParseResult, decodedPost) {
	t.Helper()
	ad, ok := TikHubAdapters()[slug]
	if !ok {
		t.Fatalf("没有适配器 %s", slug)
	}
	res, err := ad.Decode(fixture(t, fixtureName), tg)
	var inc *incompleteError
	if err != nil && !errors.As(err, &inc) {
		t.Fatalf("%s 解码失败: %v", slug, err)
	}
	var p decodedPost
	if err := json.Unmarshal(res.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return res, p
}

func target(id, resolved string) Target {
	return Target{URI: resolved, Resolved: resolved, ResourceID: id}
}

// 每个适配器都用真实样本跑一遍：拿到可用内容、作者、规整后的链接、至少一个可下载文件，
// 并且产物里没有上游的痕迹。
func TestTikHubAdaptersDecodeRealSamples(t *testing.T) {
	cases := []struct {
		slug      string
		target    Target
		kind      string
		urlPrefix string
		wantVideo bool // 期望有可下载的视频文件
		minImages int
	}{
		{"tikhub_twitter_web_fetch_tweet_detail", target("1808168603721650364", ""), "image", "https://x.com/", false, 1},
		{"tikhub_tiktok_app_v3_fetch_one_video", target("7350810998023949599", ""), "video", "https://www.tiktok.com/@jennmelon/video/", true, 0},
		{"tikhub_tiktok_app_v3_fetch_one_video_v2", target("7350810998023949599", ""), "video", "https://www.tiktok.com/@", true, 0},
		{"tikhub_tiktok_app_v3_fetch_one_video_v3", target("7350810998023949599", ""), "video", "https://www.tiktok.com/@", true, 0},
		{"tikhub_tiktok_web_fetch_post_detail", target("7350810998023949599", ""), "video", "https://www.tiktok.com/@", true, 0},
		{"tikhub_tiktok_web_fetch_post_detail_v2", target("7350810998023949599", ""), "video", "https://www.tiktok.com/@", false, 0},
		{"tikhub_douyin_app_v3_fetch_one_video", target("7448118827402972455", ""), "video", "https://www.douyin.com/video/", true, 0},
		{"tikhub_douyin_app_v3_fetch_one_video_v2", target("7448118827402972455", ""), "video", "https://www.douyin.com/video/", true, 0},
		{"tikhub_douyin_app_v3_fetch_one_video_v3", target("7448118827402972455", ""), "video", "https://www.douyin.com/video/", true, 0},
		{"tikhub_douyin_web_fetch_one_video", target("7448118827402972455", ""), "video", "https://www.douyin.com/video/", true, 0},
		{"tikhub_douyin_web_fetch_one_video_v2", target("7448118827402972455", ""), "video", "https://www.douyin.com/video/", true, 0},
		{"tikhub_xiaohongshu_app_v2_get_video_note_detail",
			target("697c0eee000000000a03c308", "https://www.xiaohongshu.com/discovery/item/697c0eee000000000a03c308?type=video&xsec_token=abc"),
			"video", "https://www.xiaohongshu.com/explore/697c0eee000000000a03c308?xsec_token=abc", true, 0},
		// 视频笔记走图文接口：只有封面，记为不完整，交给视频接口（TestIncompleteResults）
		{"tikhub_xiaohongshu_app_v2_get_image_note_detail", target("697c0eee000000000a03c308", ""), "video", "https://www.xiaohongshu.com/explore/", false, 0},
		{"tikhub_bilibili_web_fetch_one_video", target("BV1M1421t7hT", ""), "video", "https://www.bilibili.com/video/BV1M1421t7hT", false, 0},
		{"tikhub_bilibili_app_fetch_one_video", target("BV1M1421t7hT", ""), "video", "https://www.bilibili.com/video/BV1M1421t7hT", false, 0},
		{"tikhub_weibo_web_v2_fetch_post_detail", target("5092682368025584", ""), "video", "https://weibo.com/5819018196/OCT5GxFOU", true, 3},
		{"tikhub_weibo_app_fetch_status_detail", target("5092682368025584", ""), "video", "https://weibo.com/5819018196/OCT5GxFOU", true, 3},
		{"tikhub_instagram_v1_fetch_post_by_id", target("DVwbyavEa1O", ""), "video", "https://www.instagram.com/p/DVwbyavEa1O/", true, 0},
		{"tikhub_instagram_v2_fetch_post_info", target("DUajw4YkorV", ""), "image", "https://www.instagram.com/p/DUajw4YkorV/", false, 2},
		{"tikhub_instagram_v3_get_post_info", target("DUajw4YkorV", ""), "image", "https://www.instagram.com/p/DUajw4YkorV/", false, 2},
		{"tikhub_instagram_v3_get_post_info_by_code", target("DUajw4YkorV", ""), "image", "https://www.instagram.com/p/DUajw4YkorV/", false, 2},
		{"tikhub_youtube_web_v2_get_video_info", target("dQw4w9WgXcQ", ""), "video", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", false, 0},
		{"tikhub_youtube_web_v2_get_video_info_v2", target("dQw4w9WgXcQ", ""), "video", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", false, 0},
		{"tikhub_kuaishou_app_fetch_one_video_by_url", target("3xgqdzu8cnrpmqq", "https://v.kuaishou.com/cNYP0Z"), "video", "https://www.kuaishou.com/short-video/3xgqdzu8cnrpmqq", true, 0},
		{"tikhub_kuaishou_web_fetch_one_video_by_url", target("3xgqdzu8cnrpmqq", "https://v.kuaishou.com/cNYP0Z"), "video", "https://www.kuaishou.com/short-video/", true, 0},
		{"tikhub_kuaishou_web_fetch_one_video", target("3xgqdzu8cnrpmqq", "https://v.kuaishou.com/cNYP0Z"), "video", "https://www.kuaishou.com/short-video/", true, 0},
		{"tikhub_kuaishou_web_fetch_one_video_v2", target("3xtdqvdnqd3psuc", ""), "video", "https://www.kuaishou.com/short-video/3xtdqvdnqd3psuc", true, 0},
		{"tikhub_wechat_mp_v2_fetch_article_detail", target("s:TSNQKkRpN1qbKsT7BvzqIw", "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw"),
			"article", "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", false, 10},
	}
	for _, c := range cases {
		t.Run(c.slug, func(t *testing.T) {
			res, p := decodeWith(t, c.slug, strings.TrimPrefix(c.slug, "tikhub_"), c.target)
			if !usable(res) {
				t.Fatal("没有标题也没有正文")
			}
			if p.Kind != c.kind {
				t.Errorf("kind=%s，期望 %s", p.Kind, c.kind)
			}
			if !strings.HasPrefix(p.URL, c.urlPrefix) {
				t.Errorf("url=%s，期望以 %s 开头", p.URL, c.urlPrefix)
			}
			if p.Author == nil || p.Author.Name == "" {
				t.Errorf("没有作者名: %+v", p.Author)
			}
			var src map[string]any
			_ = json.Unmarshal(res.SourceData, &src)
			if src["author_name"] == nil || src["provider"] == nil {
				t.Errorf("source_data 缺作者或平台: %v", src)
			}
			var video, images int
			for _, a := range res.Assets {
				if !strings.HasPrefix(a.URL, "https://") {
					t.Errorf("文件地址不是 https: %s", a.URL)
				}
				switch a.Role {
				case "media":
					video++
				case "image":
					images++
				}
			}
			if c.wantVideo != (video == 1) {
				t.Errorf("视频文件数 %d，期望有视频=%v", video, c.wantVideo)
			}
			if images < c.minImages {
				t.Errorf("图片 %d 张，期望至少 %d", images, c.minImages)
			}
			if len(res.Assets) == 0 {
				t.Error("一个可下载文件都没有")
			}
			// 产物里不能有上游的痕迹
			blob := strings.ToLower(string(res.Payload) + string(res.SourceData))
			for _, leak := range []string{"tikhub", "request_id", "cache_url"} {
				if strings.Contains(blob, leak) {
					t.Errorf("产物里出现了上游痕迹 %q", leak)
				}
			}
		})
	}
}

// 视频作品没拿到视频文件 → 不完整（兜底链继续）；YouTube、B 站本来就没有视频文件，不算。
func TestIncompleteResults(t *testing.T) {
	a := TikHubAdapters()
	cases := []struct {
		slug, fixture string
		target        Target
		incomplete    bool
	}{
		{"tikhub_xiaohongshu_app_v2_get_image_note_detail", "xiaohongshu_app_v2_get_image_note_detail", target("697c0eee000000000a03c308", ""), true},
		{"tikhub_tiktok_web_fetch_post_detail_v2", "tiktok_web_fetch_post_detail_v2", target("7350810998023949599", ""), true},
		{"tikhub_youtube_web_v2_get_video_info", "youtube_web_v2_get_video_info", target("dQw4w9WgXcQ", ""), false},
		{"tikhub_bilibili_web_fetch_one_video", "bilibili_web_fetch_one_video", target("BV1M1421t7hT", ""), false},
		{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_fetch_one_video", target("7448118827402972455", ""), false},
	}
	for _, c := range cases {
		res, err := a[c.slug].Decode(fixture(t, c.fixture), c.target)
		var inc *incompleteError
		if got := errors.As(err, &inc); got != c.incomplete || res == nil || !usable(res) {
			t.Errorf("%s：不完整=%v（期望 %v），err=%v", c.slug, got, c.incomplete, err)
		}
	}
}

// 带视频的推文（用户提供的 ID，实测）：挑放得进 25 MiB 的最高码率 —— 45.6 秒的视频，
// 1080p（10.4 Mbps）约 59 MB 放不下，720p（2.2 Mbps）约 12 MB 正好。
func TestTweetVideoPicksVariantThatFits(t *testing.T) {
	_, p := decodeWith(t, "tikhub_twitter_web_fetch_tweet_detail", "twitter_video", target("2110919060557107441", ""))
	if p.Kind != "video" || p.Video == nil {
		t.Fatalf("应是视频作品: %+v", p)
	}
	if !strings.Contains(p.Video.URL, "/1280x720/") || p.Video.Width != 1280 || p.Video.Height != 720 || p.Video.DurationMs != 45630 {
		t.Errorf("挑的码率不对: %+v", p.Video)
	}
	if !strings.Contains(p.Cover, "amplify_video_thumb") || len(p.Images) != 0 {
		t.Errorf("封面应是视频缩略图，且不该当成图片: cover=%s images=%d", p.Cover, len(p.Images))
	}
}

// 小红书图文笔记：图文接口拿到全部图片。
func TestXHSImageNote(t *testing.T) {
	_, p := decodeWith(t, "tikhub_xiaohongshu_app_v2_get_image_note_detail", "xhs_image_on_normal", target("6a3e33b2000000001603e982", ""))
	if p.Kind != "image" || len(p.Images) == 0 || p.Title == "" {
		t.Errorf("图文笔记解码不对: kind=%s images=%d title=%q", p.Kind, len(p.Images), p.Title)
	}
}

// 「HTTP 200 但其实失败」与「上游返回了别的作品」都必须当失败，让兜底链继续。
func TestTikHubDecodeRejectsWrongOrFailed(t *testing.T) {
	adapters := TikHubAdapters()
	cases := []struct {
		name, slug, fixture string
		target              Target
		wrongItem           bool
	}{
		// 视频接口拿图文笔记 ID：返回的是推荐的别人的视频笔记
		{"小红书视频接口返回别人的笔记", "tikhub_xiaohongshu_app_v2_get_video_note_detail", "xhs_video_on_normal", target("6a3e33b2000000001603e982", ""), true},
		// Instagram v1：{"status": false, "errorMessage": "This post does not exist."}
		{"Instagram v1 status=false", "tikhub_instagram_v1_fetch_post_by_url", "instagram_v1_fetch_post_by_url", target("DUajw4YkorV", ""), false},
		{"抖音作品 ID 对不上", "tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_fetch_one_video", target("7000000000000000000", ""), true},
		{"推文 ID 对不上", "tikhub_twitter_web_fetch_tweet_detail", "twitter_web_fetch_tweet_detail", target("1", ""), true},
		{"微博 mid 对不上", "tikhub_weibo_web_v2_fetch_post_detail", "weibo_web_v2_fetch_post_detail", target("1", ""), true},
		{"快手作品 ID 对不上", "tikhub_kuaishou_web_fetch_one_video_v2", "kuaishou_web_fetch_one_video_v2", target("3xother", ""), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := adapters[c.slug].Decode(fixture(t, c.fixture), c.target)
			if err == nil {
				t.Fatal("应当失败")
			}
			if c.wrongItem && !errors.Is(err, errWrongItem) {
				t.Errorf("应是 errWrongItem，得到 %v", err)
			}
		})
	}
	// 只有 ID 的空壳（实测）：当失败交给下一个接口，不能当成内容
	if _, err := adapters["tikhub_xiaohongshu_app_v2_get_video_note_detail"].Decode(fixture(t, "xhs_gone_video_feed"),
		target("69690331000000001a02266a", "")); err == nil || errors.Is(err, errWrongItem) {
		t.Errorf("空壳应是普通解码失败，得到 %v", err)
	}
	// Instagram v1 按链接的成功形态也认得
	if _, err := adapters["tikhub_instagram_v1_fetch_post_by_url"].Decode(fixture(t, "instagram_v1_by_url_demo"), target("DPwhVB-jo9k", "")); err != nil {
		t.Errorf("v1 按链接的成功响应应能解码: %v", err)
	}
}

// 小红书视频接口只接分享链接里标明 type=video 的；B 站网页接口只认 BV 号。
func TestAdapterAccept(t *testing.T) {
	a := TikHubAdapters()
	image := a["tikhub_xiaohongshu_app_v2_get_image_note_detail"].Accept
	video := a["tikhub_xiaohongshu_app_v2_get_video_note_detail"].Accept
	typed := func(ty string) Target {
		return target("x", "https://www.xiaohongshu.com/discovery/item/x"+map[string]string{"": "", "video": "?type=video", "normal": "?type=normal"}[ty])
	}
	// type=video 只走视频；type=normal 只走图文（视频接口拿图文 ID 会返回别人的笔记）；没有 type 两个都可以
	if image(typed("video")) || !video(typed("video")) || !image(typed("normal")) || video(typed("normal")) ||
		!image(typed("")) || !video(typed("")) {
		t.Error("小红书两个接口的分流不对")
	}
	if a["tikhub_bilibili_web_fetch_one_video"].Accept(target("av170001", "")) {
		t.Error("B 站网页接口不该接 av 号")
	}
	if a["tikhub_instagram_v1_fetch_post_by_id"].Accept(target("CsomethingVeryLongPrivateCode", "")) {
		t.Error("换不出媒体 ID 的 shortcode 不该交给按 ID 的接口")
	}
}

// 所有适配器的平台都已注册，并且每个都有实测样本撑着。
func TestTikHubAdaptersAreRegisteredAndSampled(t *testing.T) {
	platforms := map[string]bool{PlatformYouTube: true}
	for _, p := range NewUpstreamPlatforms() {
		platforms[p.Provider()] = true
	}
	for slug, a := range TikHubAdapters() {
		if !platforms[a.Provider] {
			t.Errorf("%s 的平台 %s 没有注册", slug, a.Provider)
		}
		if _, err := os.Stat("testdata/tikhub/" + strings.TrimPrefix(slug, "tikhub_") + ".json"); err != nil {
			t.Errorf("%s 没有实测样本", slug)
		}
	}
}

func TestPlatformMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewYouTubeProvider())
	for _, p := range NewUpstreamPlatforms() {
		reg.Register(p)
	}
	cases := []struct{ uri, provider, id string }{
		{"https://x.com/elonmusk/status/1808168603721650364?s=20", PlatformTwitter, "1808168603721650364"},
		{"https://twitter.com/jack/status/20", PlatformTwitter, "20"},
		{"https://www.tiktok.com/@jennmelon/video/7350810998023949599?_r=1&u_code=x", PlatformTikTok, "7350810998023949599"},
		{"https://www.iesdouyin.com/share/video/6918273131559881997/?region=US", PlatformDouyin, "6918273131559881997"},
		{"https://www.douyin.com/note/7448118827402972455", PlatformDouyin, "7448118827402972455"},
		{"https://www.douyin.com/discover?modal_id=7448118827402972455", PlatformDouyin, "7448118827402972455"},
		{"https://www.xiaohongshu.com/discovery/item/69690331000000001a02266a?type=video&xsec_token=x", PlatformXiaohongshu, "69690331000000001a02266a"},
		{"https://www.xiaohongshu.com/explore/697c0eee000000000a03c308", PlatformXiaohongshu, "697c0eee000000000a03c308"},
		{"https://www.bilibili.com/video/BV1M1421t7hT/?spm_id_from=333", PlatformBilibili, "BV1M1421t7hT"},
		{"https://m.bilibili.com/video/av170001", PlatformBilibili, "av170001"},
		{"https://weibo.com/5819018196/OCT5GxFOU", PlatformWeibo, "5092682368025584"},
		{"https://m.weibo.cn/detail/5092682368025584", PlatformWeibo, "5092682368025584"},
		{"https://www.instagram.com/p/DUajw4YkorV/?img_index=1", PlatformInstagram, "DUajw4YkorV"},
		{"https://www.instagram.com/reel/DVwbyavEa1O/", PlatformInstagram, "DVwbyavEa1O"},
		{"https://v.m.chenzhongtech.com/fw/photo/3xgqdzu8cnrpmqq?cc=share_copylink", PlatformKuaishou, "3xgqdzu8cnrpmqq"},
		{"https://www.kuaishou.com/short-video/3xtdqvdnqd3psuc", PlatformKuaishou, "3xtdqvdnqd3psuc"},
		{"https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", PlatformWechatMP, "s:TSNQKkRpN1qbKsT7BvzqIw"},
		{"https://mp.weixin.qq.com/s?__biz=MzIzNjc1NzUzMw==&mid=2247780992&idx=1&sn=24a", PlatformWechatMP, "MzIzNjc1NzUzMw==:2247780992:1"},
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
	for _, uri := range []string{"https://nottiktok.com/@a/video/1", "https://weibo.com/1234567/fans", "https://www.xiaohongshu.com/user/profile/5efc6e660000000001006da1"} {
		if p, _, ok := reg.Find(uri); ok {
			t.Errorf("%s 不该被 %s 认领", uri, p.Provider())
		}
	}
	shorts := map[string]string{
		"https://v.douyin.com/L4FJNR3/":      PlatformDouyin,
		"http://xhslink.com/o/8GqargIxrko":   PlatformXiaohongshu,
		"https://v.kuaishou.com/cNYP0Z":      PlatformKuaishou,
		"https://www.kuaishou.com/f/X-f2k5K": PlatformKuaishou,
		"https://b23.tv/abc123":              PlatformBilibili,
		"https://vm.tiktok.com/ZMabc123/":    PlatformTikTok,
		"https://t.cn/A6abcd":                PlatformWeibo,
	}
	for uri, want := range shorts {
		if p, ok := reg.FindShortLink(uri); !ok || p.Provider() != want {
			t.Errorf("短链 %s 应归 %s", uri, want)
		}
	}
}

func TestIDConversions(t *testing.T) {
	// 经典样例与实测样本（OCT5GxFOU 的 mid 来自微博接口返回）
	for code, mid := range map[string]string{"z0JH2lOMb": "3501756485200075", "OCT5GxFOU": "5092682368025584", "5092682368025584": "5092682368025584"} {
		if got := weiboMid(code); got != mid {
			t.Errorf("weiboMid(%s)=%s，期望 %s", code, got, mid)
		}
	}
	// 实测样本：Instagram 按 ID 接口返回 id=3850699893338385742、shortcode=DVwbyavEa1O
	if got := instagramMediaID("DVwbyavEa1O"); got != "3850699893338385742" {
		t.Errorf("instagramMediaID=%s", got)
	}
}

func TestAssetsExpireIn(t *testing.T) {
	now := time.Unix(1792128000, 0)
	assets := []Asset{
		{URL: "https://i.ytimg.com/vi/x/maxresdefault.jpg"},                                        // 无过期标记
		{URL: "https://p3-sign.douyinpic.com/a.jpg?x-expires=1792148400&x-signature=s"},            // +20400s
		{URL: "https://v5.zjcdn.com/b0b0971e87cac5b9cd8f3bfdedd6caf8/6ad1c59b/video/tos/cn/a.mp4"}, // 0x6ad1c59b=1792132507，+4507s
		{URL: "https://instagram.fbcn2-1.fna.fbcdn.net/v/a.jpg?oe=6AD7A4FE&_nc_sid=1"},             // +数天
	}
	got := assetsExpireIn(assets, now)
	want := 4507*time.Second - expiryMargin
	if got != want {
		t.Errorf("取最早过期的减余量：得到 %s，期望 %s", got, want)
	}
	if assetsExpireIn(assets[:1], now) != 0 {
		t.Error("没有过期标记时应为 0（按全局保留期）")
	}
	if got := assetsExpireIn([]Asset{{URL: "https://a/b?Expires=1792128060"}}, now); got != time.Minute {
		t.Errorf("快过期的给 1 分钟下限，得到 %s", got)
	}
}

// 跟跳转：拿到第一个能被平台识别的地址就停，不再往下跟（小红书往下是反爬页）。
func TestHTTPResolverStopsAtFirstMatch(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/s":
			http.Redirect(w, r, "/mid", http.StatusFound)
		case "/mid":
			http.Redirect(w, r, "/item/123?type=video", http.StatusFound)
		default:
			http.Redirect(w, r, "/404-antibot", http.StatusFound) // 不该走到这里
		}
	}))
	defer srv.Close()

	got, err := newHTTPResolver().Resolve(context.Background(), srv.URL+"/s", func(u string) bool {
		return strings.Contains(u, "/item/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/item/123?type=video") || len(hits) != 2 {
		t.Errorf("得到 %s，访问了 %v", got, hits)
	}

	// 没有跳转的短链（失效、被封）报错，不当成作品页
	if _, err := newHTTPResolver().Resolve(context.Background(), srv.URL+"/item/1", func(string) bool { return false }); err == nil {
		t.Error("跳到无法识别的地址应当失败")
	}
}

// 种子迁移里的接口与代码里的适配器一一对应，并且各平台的自动售价符合预期。
// 一行没有适配器的种子会被兜底链跳过（不花钱但白占位），一个没有种子的适配器后台要手工建。
func TestTikHubSeedMatchesAdapters(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT e.slug, e.provider FROM api_endpoints e JOIN api_accounts a ON a.id = e.account_id
		WHERE a.slug = 'tikhub' AND e.slug LIKE 'tikhub\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string]string{}
	for rows.Next() {
		var slug, provider string
		if err := rows.Scan(&slug, &provider); err != nil {
			t.Fatal(err)
		}
		seeded[slug] = provider
	}
	rows.Close()

	adapters := TikHubAdapters()
	for slug, a := range adapters {
		if p, ok := seeded[slug]; !ok {
			t.Errorf("适配器 %s 没有种子行", slug)
		} else if p != a.Provider {
			t.Errorf("%s 种子平台 %s 与适配器 %s 不一致", slug, p, a.Provider)
		}
	}
	for slug := range seeded {
		if _, ok := adapters[slug]; !ok {
			t.Errorf("种子行 %s 没有适配器", slug)
		}
	}

	reg := NewRegistry()
	reg.Register(NewYouTubeProvider())
	for _, p := range NewUpstreamPlatforms() {
		reg.Register(p)
	}
	svc := NewService(Deps{Registry: reg, Extapi: extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{}, adapters),
		Adapters: adapters, DefaultPrice: 10})
	prices, err := svc.Prices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{PlatformXiaohongshu: 200, PlatformWechatMP: 200, PlatformInstagram: 40, PlatformKuaishou: 20,
		PlatformTwitter: 20, PlatformTikTok: 20, PlatformDouyin: 20, PlatformBilibili: 20, PlatformWeibo: 20, PlatformYouTube: 20}
	for _, p := range prices {
		if w, ok := want[p.Provider]; ok && p.AutoCredits != w {
			t.Errorf("%s 自动售价 %d，期望 %d", p.Provider, p.AutoCredits, w)
		}
	}
}
