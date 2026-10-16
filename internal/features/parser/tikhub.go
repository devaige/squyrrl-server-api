package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/squyrrl/api/internal/features/extapi"
)

// TikHub 适配器（ADR-080）。第一批：X、TikTok、抖音、小红书、B 站、微博、Instagram、
// YouTube、快手、公众号文章，只接「取单条作品」的接口。
//
// 每个解码器都按实测响应写（2026-10-16，样本在 testdata/tikhub/），并且：
//   - 核对返回的作品 ID —— 小红书视频接口拿图文笔记 ID 会返回别人的笔记；
//   - 识别「HTTP 200 但其实失败」—— Instagram v1 会回 {"status": false, ...}，照样计费。

const tikhubBase = "https://api.tikhub.io/api/v1/"

var errWrongItem = errors.New("上游返回的不是请求的作品")

// errGone 表示上游确认作品已不存在（删除、下架、私密）。兜底链见到它就停：
// 换个接口问同一个上游，答案不会变，只会再花一次钱。
// 只在能确定时才用 —— Instagram v1 会对存在的帖子误报「不存在」，那种不算。
var errGone = errors.New("作品不存在或已删除")

func tikhubGet(path string, query map[string]string) extapi.Request {
	return extapi.Request{URL: tikhubBase + path, Query: query}
}

// tikhub 构造一个 TikHub 适配器。
func tikhub(provider, note string, req func(Target) extapi.Request,
	decode func([]byte, Target) (*post, error)) Adapter {
	return Adapter{
		Provider: provider,
		Vendor:   extapi.VendorTikHub,
		Note:     note,
		Request:  req,
		Decode: func(data []byte, t Target) (*ParseResult, error) {
			p, err := decode(data, t)
			if err != nil {
				return nil, err
			}
			return p.result(provider), nil
		},
	}
}

func byID(path, param string) func(Target) extapi.Request {
	return func(t Target) extapi.Request { return tikhubGet(path, map[string]string{param: t.ResourceID}) }
}

// TikHubAdapters 返回全部 TikHub 适配器，按 slug 索引。slug 即后台建接口时选的那一项。
func TikHubAdapters() AdapterSet {
	return AdapterSet{
		// ---- X ----
		"tikhub_twitter_web_fetch_tweet_detail": tikhub(PlatformTwitter, "单条推文",
			byID("twitter/web/fetch_tweet_detail", "tweet_id"), decodeTweet),

		// ---- TikTok：App 版三个结构相同；网页版 v2 的视频地址要带 Cookie 才能下载，只取封面 ----
		"tikhub_tiktok_app_v3_fetch_one_video": tikhub(PlatformTikTok, "单个作品（App）",
			byID("tiktok/app/v3/fetch_one_video", "aweme_id"), decodeAwemeTikTok),
		"tikhub_tiktok_app_v3_fetch_one_video_v2": tikhub(PlatformTikTok, "单个作品（App V2）",
			byID("tiktok/app/v3/fetch_one_video_v2", "aweme_id"), decodeAwemeTikTok),
		"tikhub_tiktok_app_v3_fetch_one_video_v3": tikhub(PlatformTikTok, "单个作品（App V3）",
			byID("tiktok/app/v3/fetch_one_video_v3", "aweme_id"), decodeAwemeTikTok),
		"tikhub_tiktok_web_fetch_post_detail": tikhub(PlatformTikTok, "单个作品（网页）",
			byID("tiktok/web/fetch_post_detail", "itemId"), decodeTikTokWeb(true)),
		"tikhub_tiktok_web_fetch_post_detail_v2": tikhub(PlatformTikTok, "单个作品（网页 V2，无视频文件）",
			byID("tiktok/web/fetch_post_detail_v2", "itemId"), decodeTikTokWeb(false)),

		// ---- 抖音：网页版与 App 版都是 aweme_detail ----
		"tikhub_douyin_app_v3_fetch_one_video": tikhub(PlatformDouyin, "单个作品（App）",
			byID("douyin/app/v3/fetch_one_video", "aweme_id"), decodeAwemeDouyin),
		"tikhub_douyin_app_v3_fetch_one_video_v2": tikhub(PlatformDouyin, "单个作品（App V2）",
			byID("douyin/app/v3/fetch_one_video_v2", "aweme_id"), decodeAwemeDouyin),
		"tikhub_douyin_app_v3_fetch_one_video_v3": tikhub(PlatformDouyin, "单个作品（App V3，无版权限制）",
			byID("douyin/app/v3/fetch_one_video_v3", "aweme_id"), decodeAwemeDouyin),
		"tikhub_douyin_web_fetch_one_video": tikhub(PlatformDouyin, "单个作品（网页）",
			byID("douyin/web/fetch_one_video", "aweme_id"), decodeAwemeDouyin),
		"tikhub_douyin_web_fetch_one_video_v2": tikhub(PlatformDouyin, "单个作品（网页 V2）",
			byID("douyin/web/fetch_one_video_v2", "aweme_id"), decodeAwemeDouyin),

		// ---- 小红书：视频接口只接标明 type=video 的链接（拿图文笔记 ID 会返回别人的笔记）；
		// 图文接口什么都接，视频笔记走它只能拿到封面 ----
		"tikhub_xiaohongshu_app_v2_get_video_note_detail": withAccept(tikhub(PlatformXiaohongshu, "视频笔记（只接分享链接标明是视频的）",
			byID("xiaohongshu/app_v2/get_video_note_detail", "note_id"), decodeXHSVideoFeed), xhsIsVideo),
		"tikhub_xiaohongshu_app_v2_get_image_note_detail": tikhub(PlatformXiaohongshu, "图文笔记（视频笔记只有封面）",
			byID("xiaohongshu/app_v2/get_image_note_detail", "note_id"), decodeXHSImage),

		// ---- B 站：不带视频流，只有封面 ----
		"tikhub_bilibili_web_fetch_one_video": withAccept(tikhub(PlatformBilibili, "单个视频（网页，只认 BV 号）",
			byID("bilibili/web/fetch_one_video", "bv_id"), decodeBilibili), isBV),
		"tikhub_bilibili_app_fetch_one_video": tikhub(PlatformBilibili, "单个视频（App）",
			func(t Target) extapi.Request {
				if n, ok := strings.CutPrefix(t.ResourceID, "av"); ok {
					return tikhubGet("bilibili/app/fetch_one_video", map[string]string{"av_id": n})
				}
				return tikhubGet("bilibili/app/fetch_one_video", map[string]string{"bv_id": t.ResourceID})
			}, decodeBilibili),

		// ---- 微博 ----
		"tikhub_weibo_web_v2_fetch_post_detail": tikhub(PlatformWeibo, "单条微博（网页，含长文）",
			func(t Target) extapi.Request {
				return tikhubGet("weibo/web_v2/fetch_post_detail", map[string]string{"id": t.ResourceID, "is_get_long_text": "true"})
			}, decodeWeiboWeb),
		"tikhub_weibo_app_fetch_status_detail": tikhub(PlatformWeibo, "单条微博（App）",
			byID("weibo/app/fetch_status_detail", "status_id"), decodeWeiboApp),

		// ---- Instagram：v1 两个是 GraphQL 形态，v2 / v3 是私有 API 形态 ----
		"tikhub_instagram_v1_fetch_post_by_url": tikhub(PlatformInstagram, "帖子（v1，按链接）",
			func(t Target) extapi.Request {
				return tikhubGet("instagram/v1/fetch_post_by_url", map[string]string{"post_url": instagramURL(t.ResourceID)})
			}, decodeInstagramGraph),
		"tikhub_instagram_v1_fetch_post_by_id": withAccept(tikhub(PlatformInstagram, "帖子（v1，按 ID）",
			func(t Target) extapi.Request {
				return tikhubGet("instagram/v1/fetch_post_by_id", map[string]string{"post_id": instagramMediaID(t.ResourceID)})
			}, decodeInstagramGraph), func(t Target) bool { return instagramMediaID(t.ResourceID) != "" }),
		"tikhub_instagram_v2_fetch_post_info": tikhub(PlatformInstagram, "帖子（v2）",
			byID("instagram/v2/fetch_post_info", "code_or_url"), decodeInstagramV2),
		"tikhub_instagram_v3_get_post_info": tikhub(PlatformInstagram, "帖子（v3，按链接）",
			func(t Target) extapi.Request {
				return tikhubGet("instagram/v3/get_post_info", map[string]string{"url": instagramURL(t.ResourceID)})
			}, decodeInstagramV3),
		"tikhub_instagram_v3_get_post_info_by_code": tikhub(PlatformInstagram, "帖子（v3，按 shortcode）",
			byID("instagram/v3/get_post_info_by_code", "code"), decodeInstagramV3),

		// ---- YouTube：两个版本字段相同 ----
		"tikhub_youtube_web_v2_get_video_info": tikhub(PlatformYouTube, "视频详情",
			byID("youtube/web_v2/get_video_info", "video_id"), decodeYouTube),
		"tikhub_youtube_web_v2_get_video_info_v2": tikhub(PlatformYouTube, "视频详情（V2）",
			byID("youtube/web_v2/get_video_info_v2", "video_id"), decodeYouTube),

		// ---- 快手：App 版按分享链接取；网页版三个 ----
		"tikhub_kuaishou_app_fetch_one_video_by_url": tikhub(PlatformKuaishou, "单个作品（App，按分享链接）",
			func(t Target) extapi.Request {
				return tikhubGet("kuaishou/app/fetch_one_video_by_url", map[string]string{"share_text": t.URI})
			}, decodeKuaishouApp),
		"tikhub_kuaishou_web_fetch_one_video": tikhub(PlatformKuaishou, "单个作品（网页 V1，按分享链接）",
			func(t Target) extapi.Request {
				return tikhubGet("kuaishou/web/fetch_one_video", map[string]string{"share_text": t.URI})
			}, decodeKuaishouWeb),
		"tikhub_kuaishou_web_fetch_one_video_by_url": tikhub(PlatformKuaishou, "单个作品（网页，按链接）",
			func(t Target) extapi.Request {
				return tikhubGet("kuaishou/web/fetch_one_video_by_url", map[string]string{"url": t.URI})
			}, decodeKuaishouWeb),
		"tikhub_kuaishou_web_fetch_one_video_v2": tikhub(PlatformKuaishou, "单个作品（网页 V2，按 ID）",
			byID("kuaishou/web/fetch_one_video_v2", "photo_id"), decodeKuaishouWeb),

		// ---- 公众号文章 ----
		"tikhub_wechat_mp_v2_fetch_article_detail": tikhub(PlatformWechatMP, "公众号文章",
			func(t Target) extapi.Request {
				body, _ := json.Marshal(map[string]any{"url": t.Resolved, "raw": false})
				return extapi.Request{Method: "POST", URL: tikhubBase + "wechat_mp/v2/fetch_article_detail", Body: body}
			}, decodeWechatArticle),
	}
}

func withAccept(a Adapter, accept func(Target) bool) Adapter {
	a.Accept = accept
	return a
}

func xhsIsVideo(t Target) bool { return queryParam(t.Resolved, "type") == "video" }
func isBV(t Target) bool       { return strings.HasPrefix(t.ResourceID, "BV") }

// ======================== X ========================

type tweet struct {
	ID        flexString `json:"id"`
	Text      string     `json:"text"`
	CreatedAt string     `json:"created_at"`
	Author    struct {
		RestID     flexString `json:"rest_id"`
		Name       string     `json:"name"`
		ScreenName string     `json:"screen_name"`
		Image      string     `json:"image"`
	} `json:"author"`
	Media     map[string][]tweetMedia `json:"media"`
	Likes     flexInt                 `json:"likes"`
	Retweets  flexInt                 `json:"retweets"`
	Replies   flexInt                 `json:"replies"`
	Views     flexInt                 `json:"views"`
	Bookmarks flexInt                 `json:"bookmarks"`
}

type tweetMedia struct {
	MediaURLHTTPS string `json:"media_url_https"`
}

func decodeTweet(data []byte, t Target) (*post, error) {
	var tw tweet
	if err := json.Unmarshal(data, &tw); err != nil {
		return nil, err
	}
	if string(tw.ID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, tw.ID)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://x.com/" + nonEmpty(tw.Author.ScreenName, "i") + "/status/" + t.ResourceID,
		Text:      tw.Text,
		CreatedAt: rubyDateRFC3339(tw.CreatedAt),
		Author: &postAuthor{Name: tw.Author.Name, Handle: atHandle(tw.Author.ScreenName), ID: string(tw.Author.RestID),
			URL: profileURL("https://x.com/", tw.Author.ScreenName), Avatar: tw.Author.Image},
		Stats: &postStats{Likes: i64(int64(tw.Likes)), Comments: i64(int64(tw.Replies)),
			Shares: i64(int64(tw.Retweets)), Views: i64(int64(tw.Views)), Collects: i64(int64(tw.Bookmarks))},
	}
	// 图片 media.photo；视频与动图只取封面图（媒体文件地址的格式还没有实测样本）
	for _, kind := range []string{"photo", "video", "animated_gif"} {
		for _, m := range tw.Media[kind] {
			if m.MediaURLHTTPS != "" {
				p.Images = append(p.Images, postImage{URL: m.MediaURLHTTPS})
			}
		}
	}
	return p, nil
}

// ======================== 抖音 / TikTok（aweme） ========================

type urlList struct {
	URLList []string `json:"url_list"`
	Width   int      `json:"width"`
	Height  int      `json:"height"`
}

type aweme struct {
	AwemeID    flexString `json:"aweme_id"`
	Desc       string     `json:"desc"`
	CreateTime int64      `json:"create_time"`
	Author     struct {
		Nickname    string     `json:"nickname"`
		UniqueID    string     `json:"unique_id"`
		ShortID     flexString `json:"short_id"`
		SecUID      string     `json:"sec_uid"`
		UID         flexString `json:"uid"`
		AvatarThumb urlList    `json:"avatar_thumb"`
	} `json:"author"`
	Video struct {
		Cover    urlList `json:"cover"`
		PlayAddr urlList `json:"play_addr"`
		Duration int64   `json:"duration"` // 毫秒
		Width    int     `json:"width"`
		Height   int     `json:"height"`
	} `json:"video"`
	Images        []urlList `json:"images"` // 抖音图文
	ImagePostInfo *struct {
		Images []struct {
			DisplayImage urlList `json:"display_image"`
		} `json:"images"`
	} `json:"image_post_info"` // TikTok 图文
	Statistics struct {
		Digg    flexInt `json:"digg_count"`
		Comment flexInt `json:"comment_count"`
		Share   flexInt `json:"share_count"`
		Collect flexInt `json:"collect_count"`
		Play    flexInt `json:"play_count"`
	} `json:"statistics"`
}

func decodeAweme(data []byte, t Target) (*aweme, *post, error) {
	var env struct {
		AwemeDetail *aweme `json:"aweme_detail"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil, err
	}
	a := env.AwemeDetail
	if a == nil {
		return nil, nil, errors.New("没有 aweme_detail")
	}
	if string(a.AwemeID) != t.ResourceID {
		return nil, nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, a.AwemeID)
	}
	p := &post{
		ID:        t.ResourceID,
		Text:      a.Desc,
		CreatedAt: unixRFC3339(a.CreateTime),
		Author:    &postAuthor{Name: a.Author.Nickname, ID: nonEmpty(a.Author.SecUID, string(a.Author.UID)), Avatar: firstURL(a.Author.AvatarThumb.URLList)},
		Cover:     firstURL(a.Video.Cover.URLList),
		Stats: &postStats{Likes: i64(int64(a.Statistics.Digg)), Comments: i64(int64(a.Statistics.Comment)),
			Shares: i64(int64(a.Statistics.Share)), Collects: i64(int64(a.Statistics.Collect))},
	}
	if a.Statistics.Play > 0 {
		p.Stats.Views = i64(int64(a.Statistics.Play))
	}
	for _, img := range a.Images {
		if u := firstURL(img.URLList); u != "" {
			p.Images = append(p.Images, postImage{URL: u, Width: img.Width, Height: img.Height})
		}
	}
	if a.ImagePostInfo != nil {
		for _, img := range a.ImagePostInfo.Images {
			if u := firstURL(img.DisplayImage.URLList); u != "" {
				p.Images = append(p.Images, postImage{URL: u, Width: img.DisplayImage.Width, Height: img.DisplayImage.Height})
			}
		}
	}
	if len(p.Images) == 0 {
		p.Video = &postVideo{URL: firstURL(a.Video.PlayAddr.URLList), DurationMs: a.Video.Duration,
			Width: a.Video.Width, Height: a.Video.Height}
	}
	return a, p, nil
}

func decodeAwemeTikTok(data []byte, t Target) (*post, error) {
	a, p, err := decodeAweme(data, t)
	if err != nil {
		return nil, err
	}
	p.Author.Handle = atHandle(a.Author.UniqueID)
	p.Author.URL = profileURL("https://www.tiktok.com/@", a.Author.UniqueID)
	p.URL = "https://www.tiktok.com/@" + nonEmpty(a.Author.UniqueID, "_") + "/video/" + t.ResourceID
	return p, nil
}

func decodeAwemeDouyin(data []byte, t Target) (*post, error) {
	a, p, err := decodeAweme(data, t)
	if err != nil {
		return nil, err
	}
	p.Author.Handle = nonEmpty(a.Author.UniqueID, string(a.Author.ShortID))
	if a.Author.SecUID != "" {
		p.Author.URL = "https://www.douyin.com/user/" + a.Author.SecUID
	}
	kind := "video"
	if len(p.Images) > 0 {
		kind = "note"
	}
	p.URL = "https://www.douyin.com/" + kind + "/" + t.ResourceID
	return p, nil
}

type tiktokWebItem struct {
	ID         flexString `json:"id"`
	Desc       string     `json:"desc"`
	CreateTime flexInt    `json:"createTime"`
	Author     struct {
		ID          flexString `json:"id"`
		Nickname    string     `json:"nickname"`
		UniqueID    string     `json:"uniqueId"`
		AvatarThumb string     `json:"avatarThumb"`
	} `json:"author"`
	Video struct {
		Cover    string `json:"cover"`
		PlayAddr string `json:"playAddr"`
		Duration int64  `json:"duration"` // 秒
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	} `json:"video"`
	ImagePost *struct {
		Images []struct {
			ImageURL struct {
				URLList []string `json:"urlList"`
			} `json:"imageURL"`
			ImageWidth  int `json:"imageWidth"`
			ImageHeight int `json:"imageHeight"`
		} `json:"images"`
	} `json:"imagePost"`
	Stats struct {
		DiggCount    flexInt `json:"diggCount"`
		CommentCount flexInt `json:"commentCount"`
		ShareCount   flexInt `json:"shareCount"`
		PlayCount    flexInt `json:"playCount"`
		CollectCount flexInt `json:"collectCount"`
	} `json:"stats"`
}

func decodeTikTokWeb(withVideo bool) func([]byte, Target) (*post, error) {
	return func(data []byte, t Target) (*post, error) {
		var env struct {
			ItemInfo struct {
				ItemStruct *tiktokWebItem `json:"itemStruct"`
			} `json:"itemInfo"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		it := env.ItemInfo.ItemStruct
		if it == nil {
			return nil, errors.New("没有 itemStruct")
		}
		if string(it.ID) != t.ResourceID {
			return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, it.ID)
		}
		p := &post{
			ID:        t.ResourceID,
			URL:       "https://www.tiktok.com/@" + nonEmpty(it.Author.UniqueID, "_") + "/video/" + t.ResourceID,
			Text:      it.Desc,
			CreatedAt: unixRFC3339(int64(it.CreateTime)),
			Author: &postAuthor{Name: it.Author.Nickname, Handle: atHandle(it.Author.UniqueID), ID: string(it.Author.ID),
				URL: profileURL("https://www.tiktok.com/@", it.Author.UniqueID), Avatar: it.Author.AvatarThumb},
			Cover: it.Video.Cover,
			Stats: &postStats{Likes: i64(int64(it.Stats.DiggCount)), Comments: i64(int64(it.Stats.CommentCount)),
				Shares: i64(int64(it.Stats.ShareCount)), Views: i64(int64(it.Stats.PlayCount)), Collects: i64(int64(it.Stats.CollectCount))},
		}
		if it.ImagePost != nil {
			for _, img := range it.ImagePost.Images {
				if u := firstURL(img.ImageURL.URLList); u != "" {
					p.Images = append(p.Images, postImage{URL: u, Width: img.ImageWidth, Height: img.ImageHeight})
				}
			}
		}
		if len(p.Images) == 0 {
			p.Kind = "video"
			p.Video = &postVideo{DurationMs: it.Video.Duration * 1000, Width: it.Video.Width, Height: it.Video.Height}
			if withVideo {
				p.Video.URL = it.Video.PlayAddr
			}
		}
		return p, nil
	}
}

// ======================== 小红书 ========================

type xhsNote struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"` // normal | video
	Title          string  `json:"title"`
	Desc           string  `json:"desc"`
	Time           int64   `json:"time"`
	LastUpdateTime int64   `json:"last_update_time"`
	LikedCount     flexInt `json:"liked_count"`
	CollectedCount flexInt `json:"collected_count"`
	CommentsCount  flexInt `json:"comments_count"`
	SharedCount    flexInt `json:"shared_count"`
	User           struct {
		Nickname string `json:"nickname"`
		UserID   string `json:"userid"`
		RedID    string `json:"red_id"`
		Image    string `json:"image"`
	} `json:"user"`
	ImagesList []struct {
		URL      string `json:"url"`
		Original string `json:"original"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	} `json:"images_list"`
	VideoInfoV2 *struct {
		Media struct {
			Video struct {
				Duration int64 `json:"duration"` // 秒
			} `json:"video"`
			Stream map[string][]struct {
				MasterURL string `json:"master_url"`
				Width     int    `json:"width"`
				Height    int    `json:"height"`
				Duration  int64  `json:"duration"` // 毫秒
			} `json:"stream"`
		} `json:"media"`
	} `json:"video_info_v2"`
}

func decodeXHSImage(data []byte, t Target) (*post, error) {
	var env struct {
		Data []struct {
			NoteList []xhsNote `json:"note_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 || len(env.Data[0].NoteList) == 0 {
		return nil, errors.New("没有 note_list")
	}
	n := env.Data[0].NoteList[0]
	if n.ID != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, n.ID)
	}
	return xhsPost(n, t)
}

// decodeXHSVideoFeed：视频接口返回的是一串笔记（请求的那条 + 推荐），必须按 ID 找。
func decodeXHSVideoFeed(data []byte, t Target) (*post, error) {
	var env struct {
		Data []xhsNote `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	for _, n := range env.Data {
		if n.ID == t.ResourceID {
			return xhsPost(n, t)
		}
	}
	return nil, fmt.Errorf("%w：返回的 %d 条笔记里没有 %s", errWrongItem, len(env.Data), t.ResourceID)
}

// xhsGone：笔记已删除或不可见时，两个接口都只回一个带 ID 的空壳 ——
// 没有类型、没有作者、没有标题正文图片（实测 2026-10-16，两次都计费）。
func xhsGone(n xhsNote) bool {
	return n.Type == "" && n.User.UserID == "" && n.Title == "" && n.Desc == "" && len(n.ImagesList) == 0
}

func xhsPost(n xhsNote, t Target) (*post, error) {
	if xhsGone(n) {
		return nil, errGone
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://www.xiaohongshu.com/explore/" + t.ResourceID,
		Title:     n.Title,
		Text:      n.Desc,
		CreatedAt: unixRFC3339(n.Time),
		Author: &postAuthor{Name: n.User.Nickname, Handle: n.User.RedID, ID: n.User.UserID, Avatar: n.User.Image,
			URL: profileURL("https://www.xiaohongshu.com/user/profile/", n.User.UserID)},
		Stats: &postStats{Likes: i64(int64(n.LikedCount)), Comments: i64(int64(n.CommentsCount)),
			Shares: i64(int64(n.SharedCount)), Collects: i64(int64(n.CollectedCount))},
	}
	// 网页端打开笔记要 xsec_token，分享链接里带着就保留
	if tok := queryParam(t.Resolved, "xsec_token"); tok != "" {
		p.URL += "?xsec_token=" + url.QueryEscape(tok)
	}
	if n.LastUpdateTime > 0 {
		p.version = strconv.FormatInt(n.LastUpdateTime, 10)
	}
	var images []postImage
	for _, img := range n.ImagesList {
		if u := nonEmpty(img.Original, img.URL); u != "" {
			images = append(images, postImage{URL: u, Width: img.Width, Height: img.Height})
		}
	}
	if n.Type == "video" {
		p.Kind = "video"
		if c, ok := first(images); ok {
			p.Cover = c.URL
		}
		p.Video = &postVideo{}
		if v := n.VideoInfoV2; v != nil {
			p.Video.DurationMs = v.Media.Video.Duration * 1000
			// h264 兼容性最好，没有再退到 h265
			for _, codec := range []string{"h264", "h265"} {
				if s, ok := first(v.Media.Stream[codec]); ok && s.MasterURL != "" {
					p.Video.URL, p.Video.Width, p.Video.Height = httpsURL(s.MasterURL), s.Width, s.Height
					if s.Duration > 0 {
						p.Video.DurationMs = s.Duration
					}
					break
				}
			}
		}
	} else {
		p.Images = images
	}
	return p, nil
}

// ======================== B 站 ========================

type biliVideo struct {
	Bvid     string  `json:"bvid"`
	Aid      flexInt `json:"aid"`
	Title    string  `json:"title"`
	Desc     string  `json:"desc"`
	Pic      string  `json:"pic"`
	Pubdate  int64   `json:"pubdate"`
	Duration int64   `json:"duration"` // 秒
	Owner    struct {
		Mid  flexString `json:"mid"`
		Name string     `json:"name"`
		Face string     `json:"face"`
	} `json:"owner"`
	Stat struct {
		View     flexInt `json:"view"`
		Like     flexInt `json:"like"`
		Reply    flexInt `json:"reply"`
		Favorite flexInt `json:"favorite"`
		Share    flexInt `json:"share"`
	} `json:"stat"`
	Dimension struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"dimension"`
}

func decodeBilibili(data []byte, t Target) (*post, error) {
	var env struct {
		Code    int        `json:"code"`
		Message string     `json:"message"`
		Data    *biliVideo `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 || env.Data == nil {
		return nil, fmt.Errorf("B 站返回 code=%d %s", env.Code, env.Message)
	}
	v := env.Data
	switch {
	case strings.HasPrefix(t.ResourceID, "av"):
		if "av"+strconv.FormatInt(int64(v.Aid), 10) != t.ResourceID {
			return nil, fmt.Errorf("%w：要 %s，拿到 av%d", errWrongItem, t.ResourceID, v.Aid)
		}
	case v.Bvid != "" && v.Bvid != t.ResourceID:
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, v.Bvid)
	}
	id := nonEmpty(v.Bvid, t.ResourceID)
	return &post{
		ID:        id,
		URL:       "https://www.bilibili.com/video/" + id,
		Kind:      "video",
		Title:     v.Title,
		Text:      v.Desc,
		CreatedAt: unixRFC3339(v.Pubdate),
		Author: &postAuthor{Name: v.Owner.Name, ID: string(v.Owner.Mid), Avatar: httpsURL(v.Owner.Face),
			URL: profileURL("https://space.bilibili.com/", string(v.Owner.Mid))},
		Cover: httpsURL(v.Pic),
		Video: &postVideo{DurationMs: v.Duration * 1000, Width: v.Dimension.Width, Height: v.Dimension.Height},
		Stats: &postStats{Likes: i64(int64(v.Stat.Like)), Comments: i64(int64(v.Stat.Reply)), Shares: i64(int64(v.Stat.Share)),
			Views: i64(int64(v.Stat.View)), Collects: i64(int64(v.Stat.Favorite))},
	}, nil
}

// ======================== 微博 ========================

type weiboPic struct {
	Largest  weiboImg `json:"largest"`
	Original weiboImg `json:"original"`
	Large    weiboImg `json:"large"`
}

type weiboImg struct {
	URL    string  `json:"url"`
	Width  flexInt `json:"width"`
	Height flexInt `json:"height"`
}

// UnmarshalJSON 同时认 {"url": ...} 和裸字符串：混排里视频的 page_pic 是字符串（实测）。
func (w *weiboImg) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &w.URL)
	}
	type plain weiboImg
	return json.Unmarshal(b, (*plain)(w))
}

type weiboMedia struct {
	PagePic   weiboImg `json:"page_pic"`
	MediaInfo struct {
		StreamURLHD string  `json:"stream_url_hd"`
		StreamURL   string  `json:"stream_url"`
		MP4HDURL    string  `json:"mp4_hd_url"`
		MP4SDURL    string  `json:"mp4_sd_url"`
		Duration    float64 `json:"duration"` // 秒
	} `json:"media_info"`
}

func (m weiboMedia) videoURL() string {
	return httpsURL(nonEmpty(m.MediaInfo.MP4HDURL, m.MediaInfo.StreamURLHD, m.MediaInfo.MP4SDURL, m.MediaInfo.StreamURL))
}

type weiboStatus struct {
	Mid        flexString `json:"mid"`
	IDStr      string     `json:"idstr"`
	MblogID    string     `json:"mblogid"`
	CreatedAt  string     `json:"created_at"`
	TextRaw    string     `json:"text_raw"`
	Text       string     `json:"text"`
	IsLongText bool       `json:"isLongText"`
	LongText   *struct {
		LongTextContent string `json:"longTextContent"`
	} `json:"longText"`
	EditCount *int `json:"edit_count"`
	User      struct {
		IDStr           string `json:"idstr"`
		ScreenName      string `json:"screen_name"`
		AvatarHD        string `json:"avatar_hd"`
		ProfileImageURL string `json:"profile_image_url"`
	} `json:"user"`
	PicIDs       []string            `json:"pic_ids"`
	PicInfos     map[string]weiboPic `json:"pic_infos"`
	MixMediaInfo *struct {
		Items []struct {
			Type string          `json:"type"` // pic | video
			Data json.RawMessage `json:"data"`
		} `json:"items"`
	} `json:"mix_media_info"`
	PageInfo *struct {
		ObjectType string `json:"object_type"`
		weiboMedia
	} `json:"page_info"`
	Reposts   flexInt `json:"reposts_count"`
	Comments  flexInt `json:"comments_count"`
	Attitudes flexInt `json:"attitudes_count"`
}

func decodeWeiboWeb(data []byte, t Target) (*post, error) {
	var s weiboStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return weiboPost(s, t)
}

func decodeWeiboApp(data []byte, t Target) (*post, error) {
	var env struct {
		DetailInfo struct {
			Status *weiboStatus `json:"status"`
		} `json:"detailInfo"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.DetailInfo.Status == nil {
		return nil, errors.New("没有 detailInfo.status")
	}
	return weiboPost(*env.DetailInfo.Status, t)
}

func weiboPost(s weiboStatus, t Target) (*post, error) {
	mid := nonEmpty(string(s.Mid), s.IDStr)
	if mid != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, mid)
	}
	text := nonEmpty(s.TextRaw, plainText(s.Text))
	if s.IsLongText && s.LongText != nil && s.LongText.LongTextContent != "" {
		text = plainText(s.LongText.LongTextContent)
	}
	p := &post{
		ID:        mid,
		URL:       "https://m.weibo.cn/detail/" + mid,
		Text:      text,
		CreatedAt: rubyDateRFC3339(s.CreatedAt),
		Author: &postAuthor{Name: s.User.ScreenName, ID: s.User.IDStr, Avatar: nonEmpty(s.User.AvatarHD, s.User.ProfileImageURL),
			URL: profileURL("https://weibo.com/u/", s.User.IDStr)},
		Stats: &postStats{Likes: i64(int64(s.Attitudes)), Comments: i64(int64(s.Comments)), Shares: i64(int64(s.Reposts))},
	}
	if s.User.IDStr != "" && s.MblogID != "" {
		p.URL = "https://weibo.com/" + s.User.IDStr + "/" + s.MblogID
	}
	if s.EditCount != nil {
		p.version = strconv.Itoa(*s.EditCount) // 每次编辑 +1，正好是「内容变则变」
	}

	addPic := func(pic weiboPic) {
		img := pic.Largest
		if img.URL == "" {
			img = pic.Original
		}
		if img.URL != "" {
			p.Images = append(p.Images, postImage{URL: httpsURL(img.URL), Width: int(img.Width), Height: int(img.Height)})
		}
	}
	addVideo := func(m weiboMedia) {
		if p.Video == nil && m.videoURL() != "" {
			p.Video = &postVideo{URL: m.videoURL(), DurationMs: int64(m.MediaInfo.Duration * 1000)}
			p.Cover = httpsURL(m.PagePic.URL)
		}
	}
	switch {
	case s.MixMediaInfo != nil && len(s.MixMediaInfo.Items) > 0:
		// 图片与视频混排：图片照常收，视频取第一个
		for _, it := range s.MixMediaInfo.Items {
			switch it.Type {
			case "pic":
				var pic weiboPic
				if json.Unmarshal(it.Data, &pic) == nil {
					addPic(pic)
				}
			case "video":
				var m weiboMedia
				if json.Unmarshal(it.Data, &m) == nil {
					addVideo(m)
				}
			}
		}
	default:
		for _, pid := range s.PicIDs { // pic_infos 是 map，顺序以 pic_ids 为准
			if pic, ok := s.PicInfos[pid]; ok {
				addPic(pic)
			}
		}
		if s.PageInfo != nil && s.PageInfo.ObjectType == "video" {
			addVideo(s.PageInfo.weiboMedia)
		}
	}
	if p.Video != nil {
		p.Kind = "video"
	}
	return p, nil
}

// ======================== Instagram ========================

func instagramURL(code string) string { return "https://www.instagram.com/p/" + code + "/" }

// GraphQL 形态（v1 按链接 / 按 ID）
type igGraphNode struct {
	Shortcode  string `json:"shortcode"`
	DisplayURL string `json:"display_url"`
	IsVideo    bool   `json:"is_video"`
	VideoURL   string `json:"video_url"`
	Dimensions struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"dimensions"`
}

type igGraph struct {
	Status       *bool  `json:"status"`
	ErrorMessage string `json:"errorMessage"`
	igGraphNode
	TakenAt       int64   `json:"taken_at_timestamp"`
	VideoDuration float64 `json:"video_duration"`
	Owner         struct {
		ID            flexString `json:"id"`
		Username      string     `json:"username"`
		FullName      string     `json:"full_name"`
		ProfilePicURL string     `json:"profile_pic_url"`
	} `json:"owner"`
	Caption struct {
		Edges []struct {
			Node struct {
				Text string `json:"text"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"edge_media_to_caption"`
	Children *struct {
		Edges []struct {
			Node igGraphNode `json:"node"`
		} `json:"edges"`
	} `json:"edge_sidecar_to_children"`
	Likes struct {
		Count flexInt `json:"count"`
	} `json:"edge_media_preview_like"`
	Comments struct {
		Count flexInt `json:"count"`
	} `json:"edge_media_to_parent_comment"`
	VideoViewCount flexInt `json:"video_view_count"`
}

func decodeInstagramGraph(data []byte, t Target) (*post, error) {
	var g igGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	// HTTP 200 但其实失败：{"status": false, "errorMessage": "This post does not exist."}
	if g.Status != nil && !*g.Status {
		return nil, fmt.Errorf("上游报告失败：%s", nonEmpty(g.ErrorMessage, "status=false"))
	}
	if g.Shortcode != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, g.Shortcode)
	}
	p := igBase(t, g.TakenAt, g.Owner.Username, g.Owner.FullName, string(g.Owner.ID), g.Owner.ProfilePicURL)
	if c, ok := first(g.Caption.Edges); ok {
		p.Text = c.Node.Text
	}
	p.Stats = &postStats{Likes: i64(int64(g.Likes.Count)), Comments: i64(int64(g.Comments.Count))}
	if g.VideoViewCount > 0 {
		p.Stats.Views = i64(int64(g.VideoViewCount))
	}
	nodes := []igGraphNode{g.igGraphNode}
	if g.Children != nil && len(g.Children.Edges) > 0 {
		nodes = nodes[:0]
		for _, e := range g.Children.Edges {
			nodes = append(nodes, e.Node)
		}
	}
	for _, n := range nodes {
		igAdd(p, n.IsVideo, n.DisplayURL, n.VideoURL, n.Dimensions.Width, n.Dimensions.Height, int64(g.VideoDuration*1000))
	}
	return p, nil
}

// 私有 API 形态（v2 data.data / v3 items[0]）
type igMedia struct {
	Code           string `json:"code"`
	MediaType      int    `json:"media_type"` // 1 图 2 视频 8 多图
	IsVideo        bool   `json:"is_video"`
	ImageVersions2 struct {
		Candidates []igImage `json:"candidates"`
	} `json:"image_versions2"`
	// v2 用的是 image_versions.items / video_url，v3 是 image_versions2.candidates / video_versions（实测）
	ImageVersions struct {
		Items []igImage `json:"items"`
	} `json:"image_versions"`
	VideoVersions []igImage `json:"video_versions"`
	VideoURL      string    `json:"video_url"`
	VideoDuration float64   `json:"video_duration"`
}

func (m igMedia) image() igImage {
	img, _ := first(bestIGImages(append(append([]igImage{}, m.ImageVersions2.Candidates...), m.ImageVersions.Items...)))
	return img
}

func (m igMedia) video() string {
	if v, ok := first(m.VideoVersions); ok && v.URL != "" {
		return v.URL
	}
	return m.VideoURL
}

type igImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type igPrivate struct {
	igMedia
	TakenAt int64 `json:"taken_at"`
	Caption *struct {
		Text string `json:"text"`
	} `json:"caption"`
	User struct {
		PK            flexString `json:"pk"`
		Username      string     `json:"username"`
		FullName      string     `json:"full_name"`
		ProfilePicURL string     `json:"profile_pic_url"`
	} `json:"user"`
	CarouselMedia []igMedia `json:"carousel_media"`
	LikeCount     *flexInt  `json:"like_count"`
	CommentCount  *flexInt  `json:"comment_count"`
	PlayCount     *flexInt  `json:"play_count"`
}

func decodeInstagramV2(data []byte, t Target) (*post, error) {
	var env struct {
		Data *igPrivate `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Data == nil {
		return nil, errors.New("没有 data")
	}
	return igPrivatePost(*env.Data, t)
}

func decodeInstagramV3(data []byte, t Target) (*post, error) {
	var env struct {
		Items []igPrivate `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	m, ok := first(env.Items)
	if !ok {
		return nil, errors.New("没有 items")
	}
	return igPrivatePost(m, t)
}

func igPrivatePost(m igPrivate, t Target) (*post, error) {
	if m.Code != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, m.Code)
	}
	p := igBase(t, m.TakenAt, m.User.Username, m.User.FullName, string(m.User.PK), m.User.ProfilePicURL)
	if m.Caption != nil {
		p.Text = m.Caption.Text
	}
	p.Stats = &postStats{}
	if m.LikeCount != nil {
		p.Stats.Likes = i64(int64(*m.LikeCount))
	}
	if m.CommentCount != nil {
		p.Stats.Comments = i64(int64(*m.CommentCount))
	}
	if m.PlayCount != nil {
		p.Stats.Views = i64(int64(*m.PlayCount))
	}
	items := []igMedia{m.igMedia}
	if len(m.CarouselMedia) > 0 {
		items = m.CarouselMedia
	}
	for _, it := range items {
		img := it.image()
		igAdd(p, it.MediaType == 2 || it.IsVideo, img.URL, it.video(), img.Width, img.Height, int64(it.VideoDuration*1000))
	}
	return p, nil
}

// bestIGImages 把候选尺寸按宽度降序排，第一个就是最大的。
func bestIGImages(c []igImage) []igImage {
	out := append([]igImage(nil), c...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Width > out[j].Width })
	return out
}

func igBase(t Target, takenAt int64, username, fullName, uid, avatar string) *post {
	return &post{
		ID:        t.ResourceID,
		URL:       instagramURL(t.ResourceID),
		CreatedAt: unixRFC3339(takenAt),
		Author: &postAuthor{Name: nonEmpty(fullName, username), Handle: atHandle(username), ID: uid, Avatar: avatar,
			URL: profileURL("https://www.instagram.com/", username)},
	}
}

// igAdd 收一张图或一段视频。多图帖里混着视频时，第一段视频进 Video，其余视频只收封面。
func igAdd(p *post, isVideo bool, imageURL, videoURL string, w, h int, durationMs int64) {
	if isVideo && videoURL != "" && p.Video == nil && len(p.Images) == 0 {
		p.Video = &postVideo{URL: videoURL, Width: w, Height: h, DurationMs: durationMs}
		p.Cover = imageURL
		return
	}
	if imageURL != "" {
		p.Images = append(p.Images, postImage{URL: imageURL, Width: w, Height: h})
	}
}

// ======================== YouTube ========================

type ytVideo struct {
	VideoID       string    `json:"video_id"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Author        string    `json:"author"`
	ChannelID     string    `json:"channel_id"`
	ChannelHandle string    `json:"channel_handle"`
	ChannelURL    string    `json:"channel_url"`
	PublishDate   string    `json:"publish_date"`
	LengthSeconds flexInt   `json:"length_seconds"`
	ViewCount     flexInt   `json:"view_count"`
	LikeCount     flexInt   `json:"like_count"`
	CommentCount  flexInt   `json:"comment_count"`
	Thumbnails    []igImage `json:"thumbnails"`
	Avatars       []igImage `json:"channel_avatar_thumbnails"`
}

func decodeYouTube(data []byte, t Target) (*post, error) {
	var v ytVideo
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	if v.VideoID != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, v.VideoID)
	}
	thumb, _ := first(bestIGImages(v.Thumbnails))
	avatar, _ := first(bestIGImages(v.Avatars))
	p := &post{
		ID:     t.ResourceID,
		URL:    "https://www.youtube.com/watch?v=" + t.ResourceID,
		Kind:   "video",
		Title:  v.Title,
		Text:   v.Description,
		Author: &postAuthor{Name: v.Author, Handle: v.ChannelHandle, ID: v.ChannelID, URL: v.ChannelURL, Avatar: avatar.URL},
		Cover:  thumb.URL,
		Video:  &postVideo{DurationMs: int64(v.LengthSeconds) * 1000},
		Stats:  &postStats{Likes: i64(int64(v.LikeCount)), Views: i64(int64(v.ViewCount))},
	}
	if v.CommentCount > 0 {
		p.Stats.Comments = i64(int64(v.CommentCount))
	}
	p.CreatedAt = rfc3339UTC(v.PublishDate)
	return p, nil
}

// ======================== 快手 ========================

type ksURL struct {
	URL string `json:"url"`
}

func ksFirst(xs []ksURL) string {
	u, _ := first(xs)
	return httpsURL(u.URL)
}

func decodeKuaishouApp(data []byte, t Target) (*post, error) {
	var env struct {
		Result int `json:"result"`
		Photos []struct {
			PhotoID    flexString `json:"photo_id"`
			Caption    string     `json:"caption"`
			Timestamp  int64      `json:"timestamp"`
			UserName   string     `json:"user_name"`
			UserID     flexString `json:"user_id"`
			HeadURLs   []ksURL    `json:"headurls"`
			Covers     []ksURL    `json:"cover_thumbnail_urls"`
			MainMvURLs []ksURL    `json:"main_mv_urls"`
			Duration   int64      `json:"duration"` // 毫秒
			LikeCount  flexInt    `json:"like_count"`
			Comment    flexInt    `json:"comment_count"`
			ViewCount  flexInt    `json:"view_count"`
			ShareCount flexInt    `json:"share_count"`
			Collect    flexInt    `json:"collect_count"`
			ShareInfo  string     `json:"share_info"` // userId=<eid>&photoId=<eid>
			ExtParams  struct {
				W int `json:"w"`
				H int `json:"h"`
			} `json:"ext_params"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	ph, ok := first(env.Photos)
	if env.Result != 1 || !ok {
		return nil, fmt.Errorf("快手返回 result=%d", env.Result)
	}
	share, _ := url.ParseQuery(ph.ShareInfo)
	if eid := share.Get("photoId"); eid != t.ResourceID && string(ph.PhotoID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, nonEmpty(eid, string(ph.PhotoID)))
	}
	p := ksPost(t, ph.Caption, ph.Timestamp, ph.UserName, share.Get("userId"), ksFirst(ph.HeadURLs))
	p.Cover = ksFirst(ph.Covers)
	p.Video = &postVideo{URL: ksFirst(ph.MainMvURLs), DurationMs: ph.Duration, Width: ph.ExtParams.W, Height: ph.ExtParams.H}
	p.Stats = &postStats{Likes: i64(int64(ph.LikeCount)), Comments: i64(int64(ph.Comment)), Views: i64(int64(ph.ViewCount)),
		Shares: i64(int64(ph.ShareCount)), Collects: i64(int64(ph.Collect))}
	return p, nil
}

type ksWebPhoto struct {
	PhotoID      string  `json:"photoId"`
	Caption      string  `json:"caption"`
	Timestamp    int64   `json:"timestamp"`
	UserName     string  `json:"userName"`
	UserEid      string  `json:"userEid"`
	HeadURL      string  `json:"headUrl"`
	CoverURLs    []ksURL `json:"coverUrls"`
	MainMvURLs   []ksURL `json:"mainMvUrls"`
	Duration     int64   `json:"duration"` // 毫秒
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	LikeCount    flexInt `json:"likeCount"`
	CommentCount flexInt `json:"commentCount"`
	ViewCount    flexInt `json:"viewCount"`
	ShareCount   flexInt `json:"shareCount"`
	ShareInfo    string  `json:"share_info"` // userId=<eid>&photoId=<eid>
}

// eid 是快手对外的作品 ID（链接里的那个）；photoId 字段是内部数字 ID（实测）。
func (ph ksWebPhoto) matches(id string) bool {
	share, _ := url.ParseQuery(ph.ShareInfo)
	return ph.PhotoID == id || share.Get("photoId") == id
}

// decodeKuaishouWeb 认三种外壳：{photo: {...}}、{photos: [...]}，以及网页 V1 直接给的数组。
func decodeKuaishouWeb(data []byte, t Target) (*post, error) {
	var candidates []ksWebPhoto
	var env struct {
		Result int         `json:"result"`
		Photo  *ksWebPhoto `json:"photo"`
	}
	if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		if err := json.Unmarshal(data, &candidates); err != nil {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		if env.Photo != nil {
			candidates = []ksWebPhoto{*env.Photo}
		}
	}
	for _, ph := range candidates {
		if !ph.matches(t.ResourceID) {
			continue
		}
		p := ksPost(t, ph.Caption, ph.Timestamp, ph.UserName, ph.UserEid, httpsURL(ph.HeadURL))
		p.Cover = ksFirst(ph.CoverURLs)
		p.Video = &postVideo{URL: ksFirst(ph.MainMvURLs), DurationMs: ph.Duration, Width: ph.Width, Height: ph.Height}
		p.Stats = &postStats{Likes: i64(int64(ph.LikeCount)), Comments: i64(int64(ph.CommentCount)),
			Views: i64(int64(ph.ViewCount)), Shares: i64(int64(ph.ShareCount))}
		return p, nil
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("快手没有返回作品（result=%d）", env.Result)
	}
	return nil, fmt.Errorf("%w：返回的作品里没有 %s", errWrongItem, t.ResourceID)
}

func ksPost(t Target, caption string, ts int64, userName, userEid, avatar string) *post {
	return &post{
		ID:        t.ResourceID,
		URL:       "https://www.kuaishou.com/short-video/" + t.ResourceID,
		Kind:      "video",
		Text:      caption,
		CreatedAt: unixRFC3339(ts),
		Author: &postAuthor{Name: userName, ID: userEid, Avatar: avatar,
			URL: profileURL("https://www.kuaishou.com/profile/", userEid)},
	}
}

// ======================== 公众号文章 ========================

func decodeWechatArticle(data []byte, t Target) (*post, error) {
	var env struct {
		Content *struct {
			Title        string  `json:"title"`
			Desc         string  `json:"desc"`
			NickName     string  `json:"nick_name"`
			Alias        string  `json:"alias"`
			UserName     string  `json:"user_name"`
			RoundHeadImg string  `json:"round_head_img"`
			HDHeadImg    string  `json:"hd_head_img"`
			CDNURL       string  `json:"cdn_url"`
			OriCreate    flexInt `json:"ori_create_time"`
			ContentText  string  `json:"content_text"`
			Pictures     []struct {
				CDNURL string     `json:"cdn_url"`
				Width  flexString `json:"width"`
				Height flexString `json:"height"`
			} `json:"picture_page_info_list"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	c := env.Content
	if c == nil || c.Title == "" {
		return nil, errors.New("没有文章内容")
	}
	text := c.ContentText
	if text == "" {
		text = c.Desc
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       t.Resolved,
		Kind:      "article",
		Title:     c.Title,
		Text:      text,
		CreatedAt: unixRFC3339(int64(c.OriCreate)),
		Author:    &postAuthor{Name: c.NickName, Handle: nonEmpty(c.Alias, c.UserName), Avatar: httpsURL(nonEmpty(c.RoundHeadImg, c.HDHeadImg))},
		Cover:     httpsURL(c.CDNURL),
	}
	for _, pic := range c.Pictures {
		if pic.CDNURL != "" {
			w, _ := strconv.Atoi(string(pic.Width))
			h, _ := strconv.Atoi(string(pic.Height))
			p.Images = append(p.Images, postImage{URL: httpsURL(pic.CDNURL), Width: w, Height: h})
		}
	}
	return p, nil
}

// ======================== 小工具 ========================

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func atHandle(h string) string {
	if h == "" {
		return ""
	}
	return "@" + h
}

func profileURL(prefix, id string) string {
	if id == "" {
		return ""
	}
	return prefix + id
}
