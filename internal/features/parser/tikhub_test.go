package parser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	if err != nil {
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
		// 视频笔记走图文接口：只有封面
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
			checkSample(t, res, p, sampleWant{kind: c.kind, urlPrefix: c.urlPrefix, video: c.wantVideo, minImages: c.minImages})
		})
	}
}

type sampleWant struct {
	kind, urlPrefix string
	video           bool // 期望有可下载的视频文件
	minImages       int
	noAuthor        bool // 上游不给作者（知乎问题）
	noAssets        bool // 本来就没有可下载的文件（纯文字）
}

// checkSample 是每个适配器解码真实样本后都要满足的：拿到可用内容、作者、规整后的链接、
// 期望的文件，并且产物里没有上游的痕迹。
func checkSample(t *testing.T, res *ParseResult, p decodedPost, w sampleWant) {
	t.Helper()
	if !usable(res) {
		t.Fatal("没有标题也没有正文")
	}
	if p.Kind != w.kind {
		t.Errorf("kind=%s，期望 %s", p.Kind, w.kind)
	}
	if !strings.HasPrefix(p.URL, w.urlPrefix) {
		t.Errorf("url=%s，期望以 %s 开头", p.URL, w.urlPrefix)
	}
	var src map[string]any
	_ = json.Unmarshal(res.SourceData, &src)
	if !w.noAuthor {
		if p.Author == nil || p.Author.Name == "" {
			t.Errorf("没有作者名: %+v", p.Author)
		}
		if src["author_name"] == nil {
			t.Errorf("source_data 缺作者: %v", src)
		}
	}
	if src["provider"] == nil {
		t.Errorf("source_data 缺平台: %v", src)
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
	if w.video != (video == 1) {
		t.Errorf("视频文件数 %d，期望有视频=%v", video, w.video)
	}
	if images < w.minImages {
		t.Errorf("图片 %d 张，期望至少 %d", images, w.minImages)
	}
	if len(res.Assets) == 0 && !w.noAssets {
		t.Error("一个可下载文件都没有")
	}
	// 产物里不能有上游的痕迹
	blob := strings.ToLower(string(res.Payload) + string(res.SourceData))
	for _, leak := range []string{"tikhub", "request_id", "cache_url"} {
		if strings.Contains(blob, leak) {
			t.Errorf("产物里出现了上游痕迹 %q", leak)
		}
	}
}

// 始终取最清晰的一档（用户决定 2026-10-16）：文件由客户端直接从 CDN 下载再上传，
// 不经我方服务器。每个平台都核对实测样本里最高档的分辨率。
func TestVideoPicksHighestQuality(t *testing.T) {
	cases := []struct {
		slug, fixture string
		target        Target
		w, h          int
		urlHas        string
	}{
		// 4K 25 Mbps 那档约 140 MB 也照给
		{"tikhub_twitter_web_fetch_tweet_detail", "twitter_video", target("2110919060557107441", ""), 3840, 2160, "/3840x2160/"},
		{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_fetch_one_video", target("7448118827402972455", ""), 1920, 1080, ""},
		{"tikhub_douyin_web_fetch_one_video", "douyin_web_fetch_one_video", target("7448118827402972455", ""), 3840, 2160, ""},
		// 最清晰的 720p 两档都是 ByteVC2，存下来放不了；h264 / ByteVC1 里默认档码率最高（url_key 里的 1020692）
		{"tikhub_tiktok_app_v3_fetch_one_video", "tiktok_app_v3_fetch_one_video", target("7350810998023949599", ""), 576, 1024, ""},
		{"tikhub_tiktok_web_fetch_post_detail", "tiktok_web_fetch_post_detail", target("7350810998023949599", ""), 720, 1280, ""},
		{"tikhub_xiaohongshu_app_v2_get_video_note_detail", "xiaohongshu_app_v2_get_video_note_detail", target("697c0eee000000000a03c308", ""), 1080, 1920, ""},
		{"tikhub_weibo_web_v2_fetch_post_detail", "weibo_web_v2_fetch_post_detail", target("5092682368025584", ""), 720, 960, "label=mp4_720p"},
		{"tikhub_kuaishou_app_fetch_one_video_by_url", "kuaishou_app_fetch_one_video_by_url", target("3xgqdzu8cnrpmqq", "https://v.kuaishou.com/cNYP0Z"), 1080, 1442, ""},
		{"tikhub_kuaishou_web_fetch_one_video_by_url", "kuaishou_web_fetch_one_video_by_url", target("3xgqdzu8cnrpmqq", "https://v.kuaishou.com/cNYP0Z"), 1080, 1442, ""},
	}
	for _, c := range cases {
		_, p := decodeWith(t, c.slug, c.fixture, c.target)
		if p.Video == nil || p.Video.Width != c.w || p.Video.Height != c.h || !strings.Contains(p.Video.URL, c.urlHas) {
			t.Errorf("%s：期望 %dx%d，得到 %+v", c.slug, c.w, c.h, p.Video)
		}
	}
	// X 的图片与视频封面取原图
	_, photo := decodeWith(t, "tikhub_twitter_web_fetch_tweet_detail", "twitter_web_fetch_tweet_detail", target("1808168603721650364", ""))
	_, video := decodeWith(t, "tikhub_twitter_web_fetch_tweet_detail", "twitter_video", target("2110919060557107441", ""))
	if !strings.HasSuffix(photo.Images[0].URL, "?name=orig") || !strings.HasSuffix(video.Cover, "?name=orig") {
		t.Errorf("X 图片应取原图: %s / %s", photo.Images[0].URL, video.Cover)
	}
}

// 编码先于清晰度：ByteVC2 / H.266 存下来放不了，AV1 老 iPhone 解不了；h264 与 HEVC 同档比清晰度。
func TestCodecRank(t *testing.T) {
	for codec, want := range map[string]int{
		"h264": codecPlayable, "avc": codecPlayable, "avc1.64001f": codecPlayable, "hevc": codecPlayable,
		"h265": codecPlayable, "h265_hvc1": codecPlayable, "bytevc1": codecPlayable,
		"": codecPlayable, "venus7-cape4-v1": codecPlayable, // 认不出的按可播放算
		"av1": codecAV1, "av01.0.08M.08": codecAV1,
		"bytevc2": codecUnplayable, "h266": codecUnplayable, "vvc1": codecUnplayable,
	} {
		if got := codecRank(codec); got != want {
			t.Errorf("codecRank(%q) = %d，期望 %d", codec, got, want)
		}
	}
	if c, r := parseURLKey("v0200fg10000db082lfog65if4e6gca0_bytevc1_720p_312106"); c != "bytevc1" || r != 312106 {
		t.Errorf("url_key 解析: %q %d", c, r)
	}
	if c, r := parseURLKey("v0200fg10000db082lfog65if4e6gca0"); c != "" || r != 0 {
		t.Errorf("格式不对应返回零值: %q %d", c, r)
	}
}

// 实测（2026-10-17，抖音 7692290629665988800）：bit_rate 只给了 ByteVC2 的三档，按清晰度取出的
// 720p 存下来打不开；能放的 720p 在 play_addr_265（HEVC），默认档 play_addr 是 540p h264。
func TestAwemeSkipsByteVC2(t *testing.T) {
	ad := TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"]
	decode := func(video string) decodedPost {
		t.Helper()
		res, err := ad.Decode([]byte(`{"aweme_detail":{"aweme_id":"1","desc":"d","video":`+video+`}}`), target("1", ""))
		if err != nil {
			t.Fatal(err)
		}
		var p decodedPost
		if err := json.Unmarshal(res.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bvc2 := `"bit_rate":[
		{"bit_rate":251743,"is_bytevc1":2,"play_addr":{"url_list":["https://cdn/bvc2-540"],"width":576,"height":1024,"url_key":"v_bytevc2_540p_251743"}},
		{"bit_rate":250331,"is_bytevc1":2,"play_addr":{"url_list":["https://cdn/bvc2-720"],"width":720,"height":1280,"url_key":"v_bytevc2_720p_250331"}}]`
	h264 := `"play_addr":{"url_list":["https://cdn/h264-540"],"width":576,"height":1024,"url_key":"v_h264_540p_945629"}`
	hevc := `"play_addr_265":{"url_list":["https://cdn/hevc-720"],"width":720,"height":1280,"url_key":"v_bytevc1_720p_312106"}`

	if v := decode(`{"width":576,"height":1024,` + h264 + `,` + hevc + `,` + bvc2 + `}`).Video; v == nil || v.URL != "https://cdn/hevc-720" || v.Width != 720 {
		t.Errorf("应取 play_addr_265 的 720p HEVC，得到 %+v", v)
	}
	if v := decode(`{"width":576,"height":1024,` + h264 + `,` + bvc2 + `}`).Video; v == nil || v.URL != "https://cdn/h264-540" {
		t.Errorf("没有 HEVC 时应退到 540p h264，而不是 720p ByteVC2，得到 %+v", v)
	}
	// 只有 ByteVC2 也照给：有一份总比没有强
	if v := decode(`{"width":576,"height":1024,` + bvc2 + `}`).Video; v == nil || v.URL != "https://cdn/bvc2-720" {
		t.Errorf("只有 ByteVC2 时取其中最清晰的，得到 %+v", v)
	}
	// url_key 缺失时按 is_bytevc1 认编码
	noKey := `"bit_rate":[{"bit_rate":9,"is_bytevc1":2,"play_addr":{"url_list":["https://cdn/bvc2-1080"],"width":1080,"height":1920}}]`
	if v := decode(`{"width":576,"height":1024,` + h264 + `,` + noKey + `}`).Video; v == nil || v.URL != "https://cdn/h264-540" {
		t.Errorf("is_bytevc1=2 也应排在后面，得到 %+v", v)
	}
}

// 字节系 url_list 常把客户端解不了的格式（HEIC、VVC）排在最前：图文的图、封面都取能显示的（2026-10-18）。
func TestAwemePicksDisplayableImage(t *testing.T) {
	// 实测（抖音 7692800900575176625，图文 8 张）：每张的 url_list 前两个是 VVC 原图（.image），
	// 第三个是无水印的 1920 宽 jpeg；download_url_list 是带水印的 webp / jpeg；play_addr 是背景音乐。
	res, p := decodeWith(t, "tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_image_post", target("7692800900575176625", ""))
	checkSample(t, res, p, sampleWant{kind: "image", urlPrefix: "https://www.douyin.com/note/7692800900575176625", minImages: 8})
	if p.Video != nil {
		t.Errorf("图文作品不该有视频（play_addr 是背景音乐）: %+v", p.Video)
	}
	for _, im := range p.Images {
		if imageRank(im.URL) != 0 || strings.Contains(im.URL, "water") || im.Width != 1920 || im.Height != 1146 {
			t.Errorf("应取无水印的 1920×1146 jpeg，得到 %+v", im)
		}
	}
	for _, a := range res.Assets {
		if imageRank(a.URL) != 0 {
			t.Errorf("下载的文件里不该有解不了的格式: %s", a.URL)
		}
	}

	ad := TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"]
	images := func(imgs string) []postImage {
		t.Helper()
		res, err := ad.Decode([]byte(`{"aweme_detail":{"aweme_id":"1","desc":"d","images":[`+imgs+`]}}`), target("1", ""))
		if err != nil {
			t.Fatal(err)
		}
		var p decodedPost
		if err := json.Unmarshal(res.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Images
	}
	const heic = `"https://p3-sign.douyinpic.com/tos-cn-i-0813/a~tplv-dy-aweme-images:q75.heic?x-expires=1"`
	const webp = `"https://p3-sign.douyinpic.com/tos-cn-i-0813/a~tplv-dy-aweme-images:q75.webp?x-expires=1"`
	const mark = `"https://p3-sign.douyinpic.com/tos-cn-i-0813/a~tplv-dy-kuchen-v1-water:1920:1146:q80.jpeg?x-expires=1"`

	if im := images(`{"url_list":[` + heic + `,` + webp + `],"download_url_list":[` + mark + `]}`); len(im) != 1 || !strings.Contains(im[0].URL, ".webp?") {
		t.Errorf("应取无水印的 webp，得到 %+v", im)
	}
	if im := images(`{"url_list":[` + heic + `],"download_url_list":[` + mark + `]}`); len(im) != 1 || !strings.Contains(im[0].URL, "water") {
		t.Errorf("无水印图只有 HEIC 时应退到带水印的 jpeg，得到 %+v", im)
	}
	if im := images(`{"url_list":[` + heic + `]}`); len(im) != 1 || !isHEIC(im[0].URL) {
		t.Errorf("只有 HEIC 也照给（苹果端能显示），得到 %+v", im)
	}

	// TikTok App 的封面 url_list 是 heic、heic、jpeg（实测样本）
	res, p = decodeWith(t, "tikhub_tiktok_app_v3_fetch_one_video", "tiktok_app_v3_fetch_one_video", target("7350810998023949599", ""))
	if imageRank(p.Cover) != 0 {
		t.Errorf("封面应取 jpeg，得到 %s", p.Cover)
	}
	for _, a := range res.Assets {
		if r := imageRank(a.URL); a.Role != "media" && r != 0 {
			t.Errorf("下载的图片里不该有 HEIC: %s", a.URL)
		}
	}
}

// 抖音实况图文（2026-10-22 线上采样，作品 7699378552240020323）：一张图，url_list 是 heic、jpeg，
// images[].video 是 4.9 秒的 720p h264 动效；作品的 video.play_addr 是背景音乐。
func TestAwemeLivePhoto(t *testing.T) {
	res, p := decodeWith(t, "tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_live_photo", target("7699378552240020323", ""))
	checkSample(t, res, p, sampleWant{kind: "image", urlPrefix: "https://www.douyin.com/note/7699378552240020323", minImages: 1})
	if p.Video != nil {
		t.Errorf("实况图文不该有作品级视频（play_addr 是背景音乐）: %+v", p.Video)
	}
	if len(p.Images) != 1 {
		t.Fatalf("应有 1 张图，得到 %d", len(p.Images))
	}
	im := p.Images[0]
	if !strings.Contains(im.URL, ".jpeg?") || strings.Contains(im.URL, "water") {
		t.Errorf("实况图的静图应取无水印 jpeg，得到 %s", im.URL)
	}
	live := im.Live
	if live == nil || !strings.HasPrefix(live.URL, "https://") || live.Width != 720 || live.Height != 1280 || live.DurationMs != 4900 {
		t.Fatalf("应带 720×1280、4.9 秒的动效，得到 %+v", live)
	}
	var roles []string
	for _, a := range res.Assets {
		roles = append(roles, a.Role)
	}
	if strings.Join(roles, ",") != "image,motion" {
		t.Fatalf("动效应紧跟在静图后面，得到 %v", roles)
	}
	if m := res.Assets[1]; m.URL != live.URL || m.Mime != "video/mp4" {
		t.Errorf("动效文件不对: %+v", m)
	}

	ad := TikHubAdapters()["tikhub_douyin_app_v3_fetch_one_video"]
	images := func(img string) []postImage {
		t.Helper()
		res, err := ad.Decode([]byte(`{"aweme_detail":{"aweme_id":"1","desc":"d","images":[`+img+`]}}`), target("1", ""))
		if err != nil {
			t.Fatal(err)
		}
		var p decodedPost
		if err := json.Unmarshal(res.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Images
	}
	const heic = `"https://p3-sign.douyinpic.com/a~tplv-dy-aweme-images-v2:1440:2560:q80.heic?x-expires=1"`
	const webp = `"https://p3-sign.douyinpic.com/a~tplv-dy-aweme-images-v2:1440:2560:q80.webp?x-expires=1"`
	const jpeg = `"https://p3-sign.douyinpic.com/a~tplv-dy-aweme-images-v2:1440:2560:q80.jpeg?x-expires=1"`
	const clip = `{"play_addr":{"url_list":["https://v.zjcdn.com/clip/?a=1"],"width":720,"height":1280,"url_key":"v0_h264_720p_598662"},"duration":3000}`
	if im := images(`{"url_list":[` + heic + `,` + webp + `,` + jpeg + `],"clip_type":5,"live_photo_type":1,"video":` + clip + `}`); len(im) != 1 || im[0].Live == nil || !strings.Contains(im[0].URL, ".jpeg?") {
		t.Errorf("有 jpeg 时静图换成 jpeg 并带动效，得到 %+v", im)
	}
	if im := images(`{"url_list":[` + heic + `,` + webp + `],"clip_type":5,"live_photo_type":1,"video":` + clip + `}`); len(im) != 1 || im[0].Live != nil || !strings.Contains(im[0].URL, ".webp?") {
		t.Errorf("没有 jpeg 静图就只存静图，得到 %+v", im)
	}
	if im := images(`{"url_list":[` + jpeg + `],"video":` + clip + `}`); len(im) != 1 || im[0].Live != nil || im[0].Video == nil {
		t.Errorf("没标明是实况、带视频的图按混排视频存，得到 %+v", im)
	}
}

// 抖音图文混排（2026-10-27 线上采样，作品 7700564727999466664）：第一张是图（clip_type=2），
// 第二张是 10 秒的 720p h264 视频（clip_type=4，url_list 是首帧）。原先两张都存成了图。
func TestAwemeSlidesVideo(t *testing.T) {
	res, p := decodeWith(t, "tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_slides_video", target("7700564727999466664", ""))
	checkSample(t, res, p, sampleWant{kind: "image", urlPrefix: "https://www.douyin.com/note/7700564727999466664", video: true, minImages: 1})
	if p.Video != nil {
		t.Errorf("图文不该有作品级视频（play_addr 是背景音乐）: %+v", p.Video)
	}
	if len(p.Images) != 2 {
		t.Fatalf("应有 2 张，得到 %d", len(p.Images))
	}
	if im := p.Images[0]; im.Video != nil || im.Live != nil || imageRank(im.URL) != 0 {
		t.Errorf("第一张应是普通的图，得到 %+v", im)
	}
	v := p.Images[1].Video
	if v == nil || !strings.HasPrefix(v.URL, "https://") || v.Width != 720 || v.Height != 1280 || v.DurationMs != 10008 {
		t.Fatalf("第二张应是 720×1280、10 秒的视频，得到 %+v", v)
	}
	var roles []string
	for _, a := range res.Assets {
		roles = append(roles, a.Role)
	}
	if strings.Join(roles, ",") != "image,thumbnail,media" {
		t.Fatalf("视频的首帧应紧挨在视频前面做缩略图，得到 %v", roles)
	}
	if th, m := res.Assets[1], res.Assets[2]; th.URL != p.Images[1].URL || imageRank(th.URL) != 0 || m.URL != v.URL || m.Mime != "video/mp4" {
		t.Errorf("首帧或视频文件不对: %+v %+v", th, m)
	}
}

func TestImageRank(t *testing.T) {
	for u, want := range map[string]int{
		"https://p3.douyinpic.com/a~tplv-dy-aweme-images-ds-rs-v1:1920:1146:q80.jpeg?x=1":                                    0,
		"https://p3.douyinpic.com/aweme/100x100/a.webp?from=1":                                                               0,
		"https://pbs.twimg.com/media/a.JPG":                                                                                  0,
		"https://p3-sign.douyinpic.com/obj/c8510002be9a3a61aad2?lk3s=1":                                                      1,
		"https://p16.tiktokcdn.com/a~tplv-tiktokx-origin.image?x=1":                                                          1,
		"https://p16.tiktokcdn.com/a~tplv-tiktokx-cropcenter-q:300:400:q70.heic?x=1":                                         2,
		"https://p5-sign.douyinpic.com/tos-cn-i-wftb4kr1i9/_offtrans__2836x1692_e8_46_vvic~tplv-wftb4kr1i9-origin.image?x=1": 2,
	} {
		if got := imageRank(u); got != want {
			t.Errorf("imageRank(%s) = %d，期望 %d", u, got, want)
		}
	}
}

// 小红书网页端复制的链接（不带 type）按 2 倍计费；App 分享的链接带 type，按原价。
func TestXHSPriceFactor(t *testing.T) {
	var xhs PriceFactorer
	for _, p := range NewUpstreamPlatforms() {
		if p.Provider() == PlatformXiaohongshu {
			xhs = p.(PriceFactorer)
		}
	}
	for resolved, want := range map[string]int64{
		"https://www.xiaohongshu.com/explore/x?xsec_token=t":           2,
		"https://www.xiaohongshu.com/discovery/item/x?type=video&a=1":  1,
		"https://www.xiaohongshu.com/discovery/item/x?type=normal&a=1": 1,
	} {
		if got := xhs.PriceFactor(target("x", resolved)); got != want {
			t.Errorf("%s 倍数 %d，期望 %d", resolved, got, want)
		}
	}
	if xhs.PriceNote() == "" {
		t.Error("后台定价页要有说明")
	}
}

// 示例：每一份都能被对应的适配器解码（失败形态不在其中），每个适配器至少有一份。
func TestBuiltinSamples(t *testing.T) {
	adapters := TikHubAdapters()
	covered := map[string]bool{}
	for _, b := range builtinSamples {
		ad, ok := adapters[b.slug]
		if !ok {
			t.Errorf("%s 没有适配器", b.slug)
			continue
		}
		data, err := readFixture(b.file)
		if err != nil {
			t.Errorf("%s 缺样本文件 %s", b.slug, b.file)
			continue
		}
		if ad.Resolve != nil {
			if _, err := ad.Resolve(data, b.fullTarget()); err != nil {
				t.Errorf("%s / %s 换不出链接: %v", b.slug, b.file, err)
			}
		} else if res, err := ad.Decode(data, b.fullTarget()); err != nil || !usable(res) {
			t.Errorf("%s / %s 解码失败: %v", b.slug, b.file, err)
		}
		covered[b.slug] = true
	}
	for slug := range adapters {
		if !covered[slug] {
			t.Errorf("%s 没有内置示例", slug)
		}
	}
}

// 小红书图文笔记：图文接口拿到全部图片。
func TestXHSImageNote(t *testing.T) {
	_, p := decodeWith(t, "tikhub_xiaohongshu_app_v2_get_image_note_detail", "xhs_image_on_normal", target("6a3e33b2000000001603e982", ""))
	if p.Kind != "image" || len(p.Images) == 0 || p.Title == "" {
		t.Errorf("图文笔记解码不对: kind=%s images=%d title=%q", p.Kind, len(p.Images), p.Title)
	}
}

// 公众号图片消息：按图文作品解码，文字取 desc（保留换行），封面不单独下载。
func TestWechatImageMessage(t *testing.T) {
	res, p := decodeWith(t, "tikhub_wechat_mp_v2_fetch_article_detail", "wechat_mp_image_post",
		target("s:gUS-ZZFZ-dxS67ylZ1j1Tw", "https://mp.weixin.qq.com/s/gUS-ZZFZ-dxS67ylZ1j1Tw"))
	checkSample(t, res, p, sampleWant{kind: "image", urlPrefix: "https://mp.weixin.qq.com/s/gUS-ZZFZ-dxS67ylZ1j1Tw", minImages: 5})
	if p.Title == "" || p.Author == nil || p.Author.Handle != "gh_ad1902868dd9" {
		t.Errorf("标题或作者不对: title=%q author=%+v", p.Title, p.Author)
	}
	if res.Description == nil || !strings.Contains(*res.Description, "\n\n🔌老款木排插座") {
		t.Errorf("文字要保留换行: %v", res.Description)
	}
	for _, a := range res.Assets {
		if a.Role == "thumbnail" {
			t.Errorf("图片消息不该单独下载封面: %s", a.URL)
		}
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

// 抖音审核中的作品（实测 2026-10-20）：四个接口都只回过滤信息，要报成「作品本身不可见」
// 带上原因，而不是当成结构变了；过滤的是别的作品时仍是普通解码失败。v3 与网页版的 filter_detail 一样。
func TestDouyinFilteredIsUnavailable(t *testing.T) {
	adapters := TikHubAdapters()
	cases := []struct{ slug, fixture, want string }{
		{"tikhub_douyin_app_v3_fetch_one_video", "douyin_filtered_list", "filter_list reason=8（审核中，仅作者可见）"},
		{"tikhub_douyin_app_v3_fetch_one_video_v2", "douyin_filtered_list", "filter_list reason=8（审核中，仅作者可见）"},
		{"tikhub_douyin_app_v3_fetch_one_video_v3", "douyin_filtered_detail", "status_audit_self_see（审核中，仅作者可见）"},
		{"tikhub_douyin_web_fetch_one_video", "douyin_filtered_detail", "status_audit_self_see（审核中，仅作者可见）"},
	}
	for _, c := range cases {
		_, err := adapters[c.slug].Decode(fixture(t, c.fixture), target("7698348670932978600", ""))
		if !errors.Is(err, errHidden) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s / %s 应是带原因 %q 的 errHidden，得到 %v", c.slug, c.fixture, c.want, err)
		}
		_, err = adapters[c.slug].Decode(fixture(t, c.fixture), target("7000000000000000000", ""))
		if err == nil || errors.Is(err, errUnavailable) {
			t.Errorf("%s / %s 过滤的是别的作品，应是普通解码失败，得到 %v", c.slug, c.fixture, err)
		}
	}

	// 没见过的原因可能只是这个接口看不到（如版权限制）：报不可见，但链要继续
	ad := adapters["tikhub_douyin_app_v3_fetch_one_video"]
	for _, body := range []string{
		`{"filter_list":[{"aweme_id":"1","reason":3}]}`,
		`{"aweme_detail":null,"filter_detail":{"aweme_id":"1","filter_reason":"status_copyright"}}`,
	} {
		_, err := ad.Decode([]byte(body), target("1", ""))
		if !errors.Is(err, errUnavailable) || errors.Is(err, errHidden) {
			t.Errorf("%s 应是 errUnavailable 而非 errHidden，得到 %v", body, err)
		}
	}
}

// 小红书视频接口只接分享链接里标明 type=video 的；B 站网页接口只认 BV 号。
func TestAdapterAccept(t *testing.T) {
	a := TikHubAdapters()
	video := a["tikhub_xiaohongshu_app_v2_get_video_note_detail"].Accept
	typed := func(ty string) Target {
		return target("x", "https://www.xiaohongshu.com/discovery/item/x"+map[string]string{"": "", "video": "?type=video", "normal": "?type=normal"}[ty])
	}
	// 视频接口拿图文笔记 ID 会返回别人的笔记：type=normal 绝不交给它；video 与没有 type 都先试它
	if !video(typed("video")) || video(typed("normal")) || !video(typed("")) {
		t.Error("小红书视频接口的分流不对")
	}
	if a["tikhub_xiaohongshu_app_v2_get_image_note_detail"].Accept != nil {
		t.Error("图文接口什么链接都接（视频接口失败时也能拿到封面与正文）")
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
	platforms := map[string]bool{PlatformYouTube: true, PlatformReddit: true} // 这两个平台不在 NewUpstreamPlatforms 里
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
		"https://xhslink.cn/o/4nWBpYQv7zz":   PlatformXiaohongshu,
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
	reg.Register(NewRedditProvider())
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
		PlatformTwitter: 20, PlatformTikTok: 20, PlatformDouyin: 20, PlatformBilibili: 20, PlatformWeibo: 20, PlatformYouTube: 20,
		// 第二批：视频号默认不启用，没有付费接口时用默认价
		PlatformZhihu: 20, PlatformThreads: 40, PlatformReddit: 20, PlatformWechatChannels: 10, PlatformToutiao: 20,
		PlatformLemon8: 20, PlatformPipixia: 20, PlatformLinkedIn: 20, PlatformTelegram: 20}
	for _, p := range prices {
		w, ok := want[p.Provider]
		delete(want, p.Provider)
		if ok && p.AutoCredits != w {
			t.Errorf("%s 自动售价 %d，期望 %d", p.Provider, p.AutoCredits, w)
		}
	}
	for provider := range want {
		t.Errorf("定价页没有平台 %s", provider)
	}
}

// 线上采样与代码里的示例比对：同结构无差异，丢了代码读的字段会被标出来。
func TestCompareAgainstExamples(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	adapters := TikHubAdapters()
	ext := extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{}, adapters)

	var epID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM api_endpoints WHERE slug = 'tikhub_twitter_web_fetch_tweet_detail'`).Scan(&epID); err != nil {
		t.Fatal(err)
	}

	repo := extapi.NewRepo(pool)
	photo := fixture(t, "twitter_web_fetch_tweet_detail")
	okResp, _ := json.Marshal(map[string]any{"status": 200, "body": map[string]any{"code": 200, "data": json.RawMessage(photo)}})
	var broken map[string]any
	_ = json.Unmarshal(photo, &broken)
	delete(broken, "author")
	badResp, _ := json.Marshal(map[string]any{"status": 200, "body": map[string]any{"code": 200, "data": broken}})
	okID, _ := repo.InsertSample(ctx, epID, "a", nil, okResp, 200, 1)
	badID, _ := repo.InsertSample(ctx, epID, "b", nil, badResp, 200, 1)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_samples WHERE id = ANY($1)`, []uuid.UUID{okID, badID}) })

	good, err := ext.GetSample(ctx, okID)
	if err != nil || good.Diff == nil || good.Diff.MissingCount != 0 || good.Diff.Baseline != 2 || good.Diff.Closest != "图片推文" {
		t.Errorf("与示例同结构不该有缺失: %+v %v", good.Diff, err)
	}
	bad, _ := ext.GetSample(ctx, badID)
	if bad.Diff == nil || !slices.Contains(bad.Diff.Missing, "author") {
		t.Errorf("丢了 author 应被标为缺失: %+v", bad.Diff)
	}
	list, examples, _ := ext.ListSamples(ctx, epID)
	for _, x := range list {
		if len(x.Response) > 0 {
			t.Error("列表不应带响应正文")
		}
	}
	if len(examples) != 2 {
		t.Errorf("推文接口应有 2 份示例（图片、视频），实有 %d", len(examples))
	}
}

// 每个适配器都声明了解码结构，代码读取的字段从 json 标签反射出来；map 只记到键为止。
func TestWatchedPaths(t *testing.T) {
	for slug := range TikHubAdapters() {
		if len(TikHubAdapters().WatchedPaths(slug)) == 0 {
			t.Errorf("%s 没有声明解码结构", slug)
		}
	}
	paths := watchedPaths(tweet{})
	for _, want := range []string{"id", "text", "author.screen_name", "media"} {
		if !slices.Contains(paths, want) {
			t.Errorf("推文的读取字段缺 %s: %v", want, paths)
		}
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "media.") {
			t.Errorf("map 的键是数据，不该展开: %s", p)
		}
	}
	if !slices.Contains(watchedPaths(xhsVideoEnvelope{}), "data[].video_info_v2.media.stream") {
		t.Error("小红书视频流路径应停在 stream（键是编码名）")
	}
}

// 两条正常的小红书视频笔记响应（不同笔记）不该互相报缺失：它们差的是直播间、音乐、字幕这类
// 代码不读的字段（实测差出 20 多个）。
func TestStructureDiffIgnoresUnreadFields(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	adapters := TikHubAdapters()
	ext := extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{}, adapters)
	repo := extapi.NewRepo(pool)

	var epID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM api_endpoints WHERE slug = 'tikhub_xiaohongshu_app_v2_get_video_note_detail'`).Scan(&epID); err != nil {
		t.Fatal(err)
	}
	resp, _ := json.Marshal(map[string]any{"status": 200, "body": map[string]any{"code": 200, "data": json.RawMessage(fixture(t, "xhs_video_on_normal"))}})
	live, _ := repo.InsertSample(ctx, epID, "y", nil, resp, 200, 0)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_samples WHERE id = $1`, live) })

	got, err := ext.GetSample(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if got.Diff.Baseline == 0 || got.Diff.MissingCount != 0 {
		t.Errorf("代码不读的字段差异不该算缺失: %+v", got.Diff)
	}
}
