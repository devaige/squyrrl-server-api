package parser

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"

	"github.com/squyrrl/api/internal/features/extapi"
)

// 内置示例：接入每个上游接口时核对过的实测响应（2026-10-16，两批都是 TikHub 返回的完整 data）。
//
// 同一批文件有两个用途：测试拿它们验证适配器；首次部署时写进后台，成为各接口的第一批
// 「示例」，线上采样的结构比对以它们为基准。之后发现新结构，在后台把线上采样存为示例即可，
// 不必回到这里加。失败形态的样本（上游空壳、v1 误报不存在）只用于测试，不进示例 ——
// 它们会把「所有示例共有的字段」压到几乎为零，结构比对就失去意义。
//
//go:embed testdata/tikhub/*.json
var tikhubFixtures embed.FS

type builtinSample struct {
	slug   string
	file   string // testdata/tikhub/<file>.json
	target Target
	note   string // 是哪种结构
}

var builtinSamples = []builtinSample{
	{"tikhub_twitter_web_fetch_tweet_detail", "twitter_web_fetch_tweet_detail", Target{ResourceID: "1808168603721650364"}, "图片推文"},
	{"tikhub_twitter_web_fetch_tweet_detail", "twitter_video", Target{ResourceID: "2110919060557107441"}, "视频推文（多档码率）"},

	{"tikhub_tiktok_app_v3_fetch_one_video", "tiktok_app_v3_fetch_one_video", Target{ResourceID: "7350810998023949599"}, "视频作品"},
	{"tikhub_tiktok_app_v3_fetch_one_video_v2", "tiktok_app_v3_fetch_one_video_v2", Target{ResourceID: "7350810998023949599"}, "视频作品"},
	{"tikhub_tiktok_app_v3_fetch_one_video_v3", "tiktok_app_v3_fetch_one_video_v3", Target{ResourceID: "7350810998023949599"}, "视频作品"},
	{"tikhub_tiktok_web_fetch_post_detail", "tiktok_web_fetch_post_detail", Target{ResourceID: "7350810998023949599"}, "视频作品"},
	{"tikhub_tiktok_web_fetch_post_detail_v2", "tiktok_web_fetch_post_detail_v2", Target{ResourceID: "7350810998023949599"}, "视频作品"},

	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_fetch_one_video", Target{ResourceID: "7448118827402972455"}, "视频作品"},
	{"tikhub_douyin_app_v3_fetch_one_video_v2", "douyin_app_v3_fetch_one_video_v2", Target{ResourceID: "7448118827402972455"}, "视频作品"},
	{"tikhub_douyin_app_v3_fetch_one_video_v3", "douyin_app_v3_fetch_one_video_v3", Target{ResourceID: "7448118827402972455"}, "视频作品"},
	{"tikhub_douyin_web_fetch_one_video", "douyin_web_fetch_one_video", Target{ResourceID: "7448118827402972455"}, "视频作品"},
	{"tikhub_douyin_web_fetch_one_video_v2", "douyin_web_fetch_one_video_v2", Target{ResourceID: "7448118827402972455"}, "视频作品"},

	{"tikhub_xiaohongshu_app_v2_get_video_note_detail", "xiaohongshu_app_v2_get_video_note_detail", Target{ResourceID: "697c0eee000000000a03c308"}, "视频笔记（返回请求的笔记加推荐）"},
	{"tikhub_xiaohongshu_app_v2_get_image_note_detail", "xhs_image_on_normal", Target{ResourceID: "6a3e33b2000000001603e982"}, "图文笔记"},
	{"tikhub_xiaohongshu_app_v2_get_image_note_detail", "xiaohongshu_app_v2_get_image_note_detail", Target{ResourceID: "697c0eee000000000a03c308"}, "视频笔记走图文接口（只有封面）"},

	{"tikhub_bilibili_web_fetch_one_video", "bilibili_web_fetch_one_video", Target{ResourceID: "BV1M1421t7hT"}, "视频"},
	{"tikhub_bilibili_app_fetch_one_video", "bilibili_app_fetch_one_video", Target{ResourceID: "BV1M1421t7hT"}, "视频"},

	{"tikhub_weibo_web_v2_fetch_post_detail", "weibo_web_v2_fetch_post_detail", Target{ResourceID: "5092682368025584"}, "图片与视频混排"},
	{"tikhub_weibo_app_fetch_status_detail", "weibo_app_fetch_status_detail", Target{ResourceID: "5092682368025584"}, "图片与视频混排"},

	{"tikhub_instagram_v1_fetch_post_by_url", "instagram_v1_by_url_demo", Target{ResourceID: "DPwhVB-jo9k"}, "单图帖子"},
	{"tikhub_instagram_v1_fetch_post_by_id", "instagram_v1_fetch_post_by_id", Target{ResourceID: "DVwbyavEa1O"}, "视频帖子"},
	{"tikhub_instagram_v2_fetch_post_info", "instagram_v2_fetch_post_info", Target{ResourceID: "DUajw4YkorV"}, "多图帖子"},
	{"tikhub_instagram_v3_get_post_info", "instagram_v3_get_post_info", Target{ResourceID: "DUajw4YkorV"}, "多图帖子"},
	{"tikhub_instagram_v3_get_post_info_by_code", "instagram_v3_get_post_info_by_code", Target{ResourceID: "DUajw4YkorV"}, "多图帖子"},

	{"tikhub_youtube_web_v2_get_video_info", "youtube_web_v2_get_video_info", Target{ResourceID: "dQw4w9WgXcQ"}, "视频"},
	{"tikhub_youtube_web_v2_get_video_info_v2", "youtube_web_v2_get_video_info_v2", Target{ResourceID: "dQw4w9WgXcQ"}, "视频"},

	{"tikhub_kuaishou_app_fetch_one_video_by_url", "kuaishou_app_fetch_one_video_by_url", Target{URI: "https://v.kuaishou.com/cNYP0Z", ResourceID: "3xgqdzu8cnrpmqq"}, "视频作品"},
	{"tikhub_kuaishou_web_fetch_one_video", "kuaishou_web_fetch_one_video", Target{URI: "https://v.kuaishou.com/cNYP0Z", ResourceID: "3xgqdzu8cnrpmqq"}, "视频作品（返回数组）"},
	{"tikhub_kuaishou_web_fetch_one_video_by_url", "kuaishou_web_fetch_one_video_by_url", Target{URI: "https://v.kuaishou.com/cNYP0Z", ResourceID: "3xgqdzu8cnrpmqq"}, "视频作品"},
	{"tikhub_kuaishou_web_fetch_one_video_v2", "kuaishou_web_fetch_one_video_v2", Target{ResourceID: "3xtdqvdnqd3psuc"}, "视频作品"},

	{"tikhub_wechat_mp_v2_fetch_article_detail", "wechat_mp_v2_fetch_article_detail",
		Target{URI: "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", Resolved: "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", ResourceID: "s:TSNQKkRpN1qbKsT7BvzqIw"}, "文章"},

	// ---- 第二批 ----
	{"tikhub_zhihu_web_fetch_answer_detail", "zhihu_web_fetch_answer_detail", Target{ResourceID: "answer/2054145988235880002"}, "回答（纯文字）"},
	{"tikhub_zhihu_web_fetch_column_article_detail", "zhihu_web_fetch_column_article_detail", Target{ResourceID: "p/669214677"}, "专栏文章（带图）"},
	{"tikhub_zhihu_web_fetch_pin_detail", "zhihu_web_fetch_pin_detail", Target{ResourceID: "pin/2050290260991534603"}, "图文想法"},
	{"tikhub_zhihu_web_fetch_question_detail", "zhihu_web_fetch_question_detail", Target{ResourceID: "question/37811449"}, "问题"},

	{"tikhub_threads_web_fetch_post_detail_v2", "threads_web_fetch_post_detail_v2", Target{ResourceID: "DPVUglOjOUu"}, "多图帖子"},
	{"tikhub_threads_web_fetch_post_detail", "threads_web_fetch_post_detail", Target{ResourceID: "C56JDdzi0RJ"}, "视频帖子"},

	{"tikhub_reddit_app_fetch_post_details", "reddit_app_fetch_post_details", Target{ResourceID: "90bu6w"}, "视频帖"},
	{"tikhub_reddit_app_fetch_post_details", "reddit_image", Target{ResourceID: "haucpf"}, "图片帖"},
	{"tikhub_reddit_app_fetch_post_details", "reddit_text", Target{ResourceID: "1x6oat7"}, "文字帖"},

	{"tikhub_wechat_channels_v2_fetch_video_detail", "wechat_channels_v2_fetch_video_detail",
		Target{URI: "https://weixin.qq.com/sph/AH3sCoIPhH", ResourceID: "AH3sCoIPhH"}, "视频作品（视频加密）"},

	{"tikhub_toutiao_web_get_article_info", "toutiao_web_get_article_info",
		Target{URI: "https://www.toutiao.com/article/7450114952884503059/", ResourceID: "7450114952884503059"}, "文章"},
	{"tikhub_toutiao_web_get_article_info", "toutiao_weitoutiao",
		Target{URI: "https://www.toutiao.com/w/1878680639539276/", ResourceID: "1878680639539276"}, "微头条（多图，正文是 JSON）"},
	{"tikhub_toutiao_web_get_video_info", "toutiao_web_get_video_info",
		Target{URI: "https://www.toutiao.com/video/7695721913519177756/", ResourceID: "7695721913519177756"}, "视频"},
	{"tikhub_toutiao_web_get_video_info", "toutiao_web_video_xigua",
		Target{URI: "https://www.ixigua.com/7354954305222377999", ResourceID: "7354954305222377999"}, "西瓜视频"},
	{"tikhub_toutiao_app_get_video_info", "toutiao_app_get_video_info",
		Target{URI: "https://www.ixigua.com/7354954305222377999", ResourceID: "7354954305222377999"}, "长视频（西瓜视频）"},
	{"tikhub_toutiao_app_get_video_info", "toutiao_app_short_video",
		Target{URI: "https://www.toutiao.com/video/7695721913519177756/", ResourceID: "7695721913519177756"}, "短视频（另一种形态）"},

	{"tikhub_lemon8_app_fetch_post_detail", "lemon8_app_fetch_post_detail", Target{ResourceID: "7445613324903006766"}, "视频作品"},
	{"tikhub_lemon8_app_fetch_post_detail", "lemon8_gallery", Target{ResourceID: "7586839908082319902"}, "图文作品（无水印图只有 HEIC）"},

	{"tikhub_pipixia_app_fetch_post_detail", "pipixia_app_fetch_post_detail", Target{ResourceID: "7411193113223371043"}, "视频作品"},

	{"tikhub_linkedin_web_v2_get_post_detail", "linkedin_web_v2_get_post_detail",
		Target{URI: "https://www.linkedin.com/posts/satyanadella_counting-down-to-a-new-build-in-san-francisco-activity-7434688896669560833-PqKW",
			ResourceID: "activity/7434688896669560833"}, "视频帖子"},

	{"tikhub_telegram_web_fetch_post_detail", "telegram_web_fetch_post_detail", Target{ResourceID: "durov/510"}, "多图消息"},
	{"tikhub_telegram_web_fetch_post_detail", "telegram_video", Target{ResourceID: "durov/546"}, "视频消息"},
	{"tikhub_telegram_web_fetch_post_detail", "telegram_video_too_big", Target{ResourceID: "durov/552"}, "大视频（没有文件，只有封面）"},
}

func (b builtinSample) fullTarget() Target {
	t := b.target
	if t.Resolved == "" {
		t.Resolved = t.URI
	}
	return t
}

func readFixture(file string) ([]byte, error) {
	return tikhubFixtures.ReadFile("testdata/tikhub/" + file + ".json")
}

// SeedSamples 把内置示例写进还没有任何示例的接口。失败只记日志 —— 示例是排查辅助，
// 不该让 API 起不来。
func SeedSamples(ctx context.Context, ext *extapi.Service, adapters AdapterSet) {
	bySlug := map[string][]extapi.SeedSample{}
	var order []string
	for _, b := range builtinSamples {
		ad, ok := adapters[b.slug]
		if !ok {
			continue
		}
		data, err := readFixture(b.file)
		if err != nil {
			slog.Warn("内置示例缺文件", "file", b.file, "err", err)
			continue
		}
		req := ad.Request(b.fullTarget())
		snapReq := map[string]any{"method": nonEmpty(req.Method, "GET"), "url": req.URL, "query": req.Query}
		if len(req.Body) > 0 {
			snapReq["body"] = json.RawMessage(req.Body)
		}
		reqJSON, _ := json.Marshal(snapReq)
		// 与线上采样同一形状 {"status", "body"}；TikHub 外壳只留 code 与 data
		respJSON, _ := json.Marshal(map[string]any{"status": 200, "body": map[string]any{"code": 200, "data": json.RawMessage(data)}})
		if _, seen := bySlug[b.slug]; !seen {
			order = append(order, b.slug)
		}
		bySlug[b.slug] = append(bySlug[b.slug], extapi.SeedSample{
			ResourceID: b.target.ResourceID, Request: reqJSON, Response: respJSON,
			Note: "接入时实测：" + b.note,
		})
	}
	total, endpoints := 0, 0
	for _, slug := range order {
		n, err := ext.SeedSamples(ctx, slug, bySlug[slug])
		if err != nil {
			slog.Warn("写入内置示例失败", "slug", slug, "err", err)
			continue
		}
		if n > 0 {
			total, endpoints = total+n, endpoints+1
		}
	}
	if total > 0 {
		slog.Info("已写入内置示例", "samples", total, "endpoints", endpoints)
	}
}
