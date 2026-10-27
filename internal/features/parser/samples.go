package parser

import (
	"embed"
	"log/slog"

	"github.com/squyrrl/api/internal/features/extapi"
)

// 示例：接入或补全每个上游接口时核对过的实测响应（首批 2026-10-16，都是 TikHub 返回的完整 data）。
//
// 同一批文件有两个用途：测试拿它们验证适配器；它们就是各接口的「示例」，后台线上采样的
// 结构比对以它们为基准（AdapterSet.Examples）。示例只在这里维护，部署即生效，后台只读：
// 发现缺失或新增时，把那条线上采样的响应精简后放进 testdata/tikhub/、登记到下面的清单
// （用户决定 2026-10-19）。线上采样的原始响应（{"status", "body"}）留在父仓库
// docs/example/<上游>/<slug>/<参数名>_<参数值>.json，这里只放精简出的 data。
// 失败形态的样本（上游空壳、v1 误报不存在）只用于测试，不进示例 —— 它们几乎没有字段，
// 任何响应和它比都「不缺」，结构比对就失去意义。
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
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_image_post", Target{ResourceID: "7692800900575176625"}, "图文作品（原图是 VVC，另有 jpeg）"},
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_video_7698165296368139707", Target{ResourceID: "7698165296368139707"}, "视频作品（带 suggest_words、common_bar_info 等，2026-10-19 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_live_photo", Target{ResourceID: "7699378552240020323"}, "实况图文（images[].video 是动效，静图另有 jpeg，2026-10-22 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_slides_video", Target{ResourceID: "7700564727999466664"}, "图文混排视频（一图一视频，视频那张 clip_type=4，带 risk_infos，2026-10-27 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_article", Target{ResourceID: "7698925399581804456"}, "文章（正文是 markdown，插图只有 HEIC，video 里是背景音乐）"},
	{"tikhub_douyin_app_v3_fetch_one_video", "douyin_app_v3_fetch_one_video_no_caption", Target{ResourceID: "7700616034386561627"}, "没配文案的视频（desc 为空，带 music_image_beats，2026-10-27 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_one_video_v2", "douyin_app_v3_fetch_one_video_v2_no_caption", Target{ResourceID: "7700616034386561627"}, "没配文案的视频（desc 为空，带 suggest_words、unified_music_group 等，2026-10-27 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_one_video_v3", "douyin_app_v3_fetch_one_video_v3_no_caption", Target{ResourceID: "7700616034386561627"}, "没配文案的视频（desc 为空，带 suggest_words、unified_music_group 等，2026-10-27 线上采样）"},
	{"tikhub_douyin_web_fetch_one_video", "douyin_web_fetch_one_video_no_caption", Target{ResourceID: "7700616034386561627"}, "没配文案的视频（desc 为空，带 suggest_words 等，2026-10-27 线上采样）"},
	{"tikhub_douyin_app_v3_fetch_share_info_by_share_code", "douyin_app_v3_fetch_share_info_by_share_code",
		Target{URI: "【刚，Claude首次设计出抗蝎毒蛋白！狂算64小时，已过实:/ 测】长按复制打开抖音，即可阅读文章 ︽︽cz9zRIbcJa9︽︽", Code: "cz9zRIbcJa9"}, "文章口令（分享者信息已抹掉）"},

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
		Target{URI: "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", Resolved: "https://mp.weixin.qq.com/s/TSNQKkRpN1qbKsT7BvzqIw", ResourceID: "s:TSNQKkRpN1qbKsT7BvzqIw"}, "文章（raw=false 精简投影）"},
	{"tikhub_wechat_mp_v2_fetch_article_detail", "wechat_mp_image_post",
		Target{URI: "https://mp.weixin.qq.com/s/gUS-ZZFZ-dxS67ylZ1j1Tw", Resolved: "https://mp.weixin.qq.com/s/gUS-ZZFZ-dxS67ylZ1j1Tw", ResourceID: "s:gUS-ZZFZ-dxS67ylZ1j1Tw"}, "图片消息（raw=true 完整 item，文字在 desc，2026-10-22 实测）"},

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
	{"tikhub_wechat_channels_v2_fetch_video_detail", "wechat_channels_video_AounEmtov2",
		Target{URI: "https://weixin.qq.com/sph/AounEmtov2", ResourceID: "AounEmtov2"}, "视频作品（带位置，视频加密，2026-10-25 实测）"},

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

// Examples 实现 extapi.Catalog：某个接口的示例，即清单里属于它的那几份实测响应。
func (s AdapterSet) Examples(slug string) []extapi.Example {
	if _, ok := s[slug]; !ok {
		return nil
	}
	var out []extapi.Example
	for _, b := range builtinSamples {
		if b.slug != slug {
			continue
		}
		data, err := readFixture(b.file)
		if err != nil {
			slog.Warn("示例缺文件", "file", b.file, "err", err)
			continue
		}
		out = append(out, extapi.Example{Note: b.note, ResourceID: b.target.ResourceID, Data: data})
	}
	return out
}
