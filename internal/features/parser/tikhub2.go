package parser

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/squyrrl/api/internal/features/extapi"
)

// TikHub 第二批适配器（2026-10-16）：知乎、Threads、Reddit、视频号、头条（含西瓜视频）、
// Lemon8、皮皮虾、LinkedIn、Telegram 公开频道。写法同第一批：按实测响应写，能核对作品 ID 的都核对。
//
// 实测后没接的接口：
//   - 西瓜视频自己的两个：v1 没有标题与作者、最高只给 720p，v2 直接报错。西瓜的作品就是头条的 group，
//     头条视频接口拿西瓜 ID 能取到标题与 1080p，所以西瓜链接归头条平台；
//   - 头条 App 版文章：没有正文，标题只有分享标题（带「 - 今日头条」），处处不如网页版。

// errOtherKind：链接指向的是另一种内容（头条的 group 链接看不出是文章还是视频），交给别的接口。
var errOtherKind = errors.New("不是这个接口处理的内容类型")

func tikhubBatch2() AdapterSet {
	return AdapterSet{
		// ---- 知乎：四种内容各一个接口，按 ID 前缀分流 ----
		"tikhub_zhihu_web_fetch_answer_detail": zhihuAdapter("answer/", "回答",
			"zhihu/web/fetch_answer_detail", "answer_id", zhihuAnswer{}, decodeZhihuAnswer),
		"tikhub_zhihu_web_fetch_column_article_detail": zhihuAdapter("p/", "专栏文章",
			"zhihu/web/fetch_column_article_detail", "article_id", zhihuArticle{}, decodeZhihuArticle),
		"tikhub_zhihu_web_fetch_pin_detail": zhihuAdapter("pin/", "想法",
			"zhihu/web/fetch_pin_detail", "pin_id", zhihuPin{}, decodeZhihuPin),
		"tikhub_zhihu_web_fetch_question_detail": zhihuAdapter("question/", "问题",
			"zhihu/web/fetch_question_detail", "question_id", zhihuQuestion{}, decodeZhihuQuestion),

		// ---- Threads：v1 要数字 ID（shortcode 换算，同 Instagram），v2 直接收 shortcode ----
		"tikhub_threads_web_fetch_post_detail": shaped(withAccept(tikhub(PlatformThreads, "帖子（按数字 ID）",
			func(t Target) extapi.Request {
				return tikhubGet("threads/web/fetch_post_detail", map[string]string{"post_id": instagramMediaID(t.ResourceID)})
			}, decodeThreadsV1), func(t Target) bool { return instagramMediaID(t.ResourceID) != "" }), threadsV1Envelope{}),
		"tikhub_threads_web_fetch_post_detail_v2": shaped(tikhub(PlatformThreads, "帖子（按 shortcode）",
			byID("threads/web/fetch_post_detail_v2", "post_id"), decodeThreadsV2), threadsV2Envelope{}),

		// ---- Reddit：排在内置免费来源（公开 JSON）之后。公开 JSON 对机房 IP 常年 403，实测本机也是 ----
		"tikhub_reddit_app_fetch_post_details": shaped(tikhub(PlatformReddit, "帖子（App）",
			func(t Target) extapi.Request {
				return tikhubGet("reddit/app/fetch_post_details", map[string]string{"post_id": "t3_" + t.ResourceID})
			}, decodeReddit), redditEnvelope{}),

		// ---- 视频号：视频文件加密（前 128 KB 要用 decode_key 解密才能播放），由客户端解开 ----
		"tikhub_wechat_channels_v2_fetch_video_detail": shaped(tikhub(PlatformWechatChannels, "作品（视频由客户端解密）",
			func(t Target) extapi.Request {
				body, _ := json.Marshal(map[string]any{"share_url": "https://weixin.qq.com/sph/" + t.ResourceID, "raw": false})
				return extapi.Request{Method: "POST", URL: tikhubBase + "wechat_channels/v2/fetch_video_detail", Body: body}
			}, decodeWechatChannels), wxChannelsVideo{}),

		// ---- 头条：文章（含微头条）走网页版文章接口；视频先网页版再 App 版（App 版长短视频两种形态，网页版一种） ----
		"tikhub_toutiao_web_get_article_info": shaped(withAccept(tikhub(PlatformToutiao, "文章 / 微头条（网页）",
			byID("toutiao/web/get_article_info", "aweme_id"), decodeToutiaoArticle),
			func(t Target) bool { return toutiaoKind(t.Resolved) != "video" }), toutiaoArticleEnvelope{}),
		"tikhub_toutiao_web_get_video_info": shaped(withAccept(tikhub(PlatformToutiao, "视频（网页，含西瓜视频）",
			byID("toutiao/web/get_video_info", "aweme_id"), decodeToutiaoWebVideo),
			func(t Target) bool { return toutiaoKind(t.Resolved) != "article" }), toutiaoWebVideoEnvelope{}),
		"tikhub_toutiao_app_get_video_info": shaped(withAccept(tikhub(PlatformToutiao, "视频（App）",
			byID("toutiao/app/get_video_info", "group_id"), decodeToutiaoAppVideo),
			func(t Target) bool { return toutiaoKind(t.Resolved) != "article" }), toutiaoAppVideoEnvelope{}),

		"tikhub_lemon8_app_fetch_post_detail": shaped(tikhub(PlatformLemon8, "作品（图文 / 视频）",
			byID("lemon8/app/fetch_post_detail", "item_id"), decodeLemon8), lemon8Envelope{}),

		"tikhub_pipixia_app_fetch_post_detail": shaped(tikhub(PlatformPipixia, "作品（视频）",
			byID("pipixia/app/fetch_post_detail", "cell_id"), decodePipixia), ppxEnvelope{}),

		// LinkedIn 只收完整链接（/posts/… 与 /pulse/…），传用户给的那条
		"tikhub_linkedin_web_v2_get_post_detail": shaped(tikhub(PlatformLinkedIn, "帖子 / 文章",
			func(t Target) extapi.Request {
				return tikhubGet("linkedin/web_v2/get_post_detail", map[string]string{"url": t.Resolved})
			}, decodeLinkedIn), linkedinPost{}),

		"tikhub_telegram_web_fetch_post_detail": shaped(tikhub(PlatformTelegram, "公开频道消息",
			func(t Target) extapi.Request {
				channel, id, _ := strings.Cut(t.ResourceID, "/")
				return tikhubGet("telegram/web/fetch_post_detail", map[string]string{"channel": channel, "post_id": id})
			}, decodeTelegram), tgPost{}),
	}
}

func shaped(a Adapter, shape any) Adapter {
	a.Shape = shape
	return a
}

// ======================== HTML 正文 ========================

var (
	htmlBlockEnd = regexp.MustCompile(`(?i)</p>|<br\s*/?>|</h[1-6]>|</li>|</blockquote>|</figure>|</div>`)
	htmlImgTag   = regexp.MustCompile(`(?i)<img\s[^>]*>`)
	htmlAttr     = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
	blankLines   = regexp.MustCompile(`\n{2,}`)
)

// htmlText 把富文本正文转成纯文本：段落、换行、标题各占一行，标签去掉、实体反转义。
func htmlText(s string) string {
	s = htmlBlockEnd.ReplaceAllString(s, "\n")
	s = html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n"))
}

// htmlImages 取正文里的图片，优先原图属性（知乎 data-original 是 _r 原图，src 是 1440w 压缩图）。
// 内嵌的 data: 占位图跳过，同一张图只收一次。
func htmlImages(s string) []postImage {
	var out []postImage
	seen := map[string]bool{}
	for _, tag := range htmlImgTag.FindAllString(s, -1) {
		attrs := map[string]string{}
		for _, m := range htmlAttr.FindAllStringSubmatch(tag, -1) {
			attrs[strings.ToLower(m[1])] = html.UnescapeString(m[2])
		}
		u := nonEmpty(attrs["data-original"], attrs["data-actualsrc"], attrs["src"])
		if !strings.HasPrefix(u, "http") || seen[u] {
			continue
		}
		seen[u] = true
		w, _ := strconv.Atoi(nonEmpty(attrs["data-rawwidth"], attrs["width"]))
		h, _ := strconv.Atoi(nonEmpty(attrs["data-rawheight"], attrs["height"]))
		out = append(out, postImage{URL: httpsURL(u), Width: w, Height: h})
	}
	return out
}

// abbrevCount 解析「3.83M」「938K」这类缩写计数（Telegram 只给这种）。
type abbrevCount int64

func (c *abbrevCount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	mul := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mul, s = 1e3, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mul, s = 1e6, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "B"):
		mul, s = 1e9, strings.TrimSuffix(s, "B")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*c = abbrevCount(math.Round(f * mul)) // 3.83*1e6 直接截断会得到 3829999
	}
	return nil
}

func optCount(n *flexInt) *int64 {
	if n == nil {
		return nil
	}
	return i64(int64(*n))
}

// ======================== 知乎 ========================

func zhihuAdapter(prefix, note, path, param string, shape any, decode func([]byte, Target) (*post, error)) Adapter {
	return shaped(withAccept(tikhub(PlatformZhihu, note,
		func(t Target) extapi.Request {
			return tikhubGet(path, map[string]string{param: strings.TrimPrefix(t.ResourceID, prefix)})
		}, decode), func(t Target) bool { return strings.HasPrefix(t.ResourceID, prefix) }), shape)
}

type zhihuAuthor struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URLToken string `json:"url_token"`
	Avatar   string `json:"avatar_url"`
}

func (a zhihuAuthor) author() *postAuthor {
	return &postAuthor{Name: a.Name, ID: a.ID, Avatar: a.Avatar, URL: profileURL("https://www.zhihu.com/people/", a.URLToken)}
}

func zhihuCheck(got flexString, t Target, prefix string) error {
	if want := strings.TrimPrefix(t.ResourceID, prefix); string(got) != want {
		return fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, want, got)
	}
	return nil
}

type zhihuAnswer struct {
	ID          flexString  `json:"id"`
	Content     string      `json:"content"`
	CreatedTime int64       `json:"created_time"`
	Author      zhihuAuthor `json:"author"`
	Question    struct {
		ID    flexString `json:"id"`
		Title string     `json:"title"`
	} `json:"question"`
	VoteupCount  *flexInt `json:"voteup_count"`
	CommentCount *flexInt `json:"comment_count"`
}

// 回答页的标题就是所属问题，网页上也是这样呈现的。
func decodeZhihuAnswer(data []byte, t Target) (*post, error) {
	var a zhihuAnswer
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	if err := zhihuCheck(a.ID, t, "answer/"); err != nil {
		return nil, err
	}
	return &post{
		ID:        string(a.ID),
		URL:       "https://www.zhihu.com/question/" + string(a.Question.ID) + "/answer/" + string(a.ID),
		Kind:      "article",
		Title:     a.Question.Title,
		Text:      htmlText(a.Content),
		CreatedAt: unixRFC3339(a.CreatedTime),
		Author:    a.Author.author(),
		Images:    htmlImages(a.Content),
		Stats:     &postStats{Likes: optCount(a.VoteupCount), Comments: optCount(a.CommentCount)},
	}, nil
}

type zhihuArticle struct {
	ID           flexString  `json:"id"`
	Title        string      `json:"title"`
	Content      string      `json:"content"`
	Created      int64       `json:"created"`
	Author       zhihuAuthor `json:"author"`
	ImageURL     string      `json:"image_url"` // 题图，没有时为空
	VoteupCount  *flexInt    `json:"voteup_count"`
	CommentCount *flexInt    `json:"comment_count"`
}

func decodeZhihuArticle(data []byte, t Target) (*post, error) {
	var a zhihuArticle
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	if err := zhihuCheck(a.ID, t, "p/"); err != nil {
		return nil, err
	}
	return &post{
		ID:        string(a.ID),
		URL:       "https://zhuanlan.zhihu.com/p/" + string(a.ID),
		Kind:      "article",
		Title:     a.Title,
		Text:      htmlText(a.Content),
		CreatedAt: unixRFC3339(a.Created),
		Author:    a.Author.author(),
		Cover:     httpsURL(a.ImageURL),
		Images:    htmlImages(a.Content),
		Stats:     &postStats{Likes: optCount(a.VoteupCount), Comments: optCount(a.CommentCount)},
	}, nil
}

type zhihuPin struct {
	ID      flexString  `json:"id"`
	Created int64       `json:"created"`
	Author  zhihuAuthor `json:"author"`
	Content []struct {
		Type        string `json:"type"` // text | image（实测）
		Content     string `json:"content"`
		OriginalURL string `json:"original_url"` // 原图；url 是 720w 带水印
		URL         string `json:"url"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	} `json:"content"`
	LikeCount     *flexInt `json:"like_count"`
	CommentCount  *flexInt `json:"comment_count"`
	RepinCount    *flexInt `json:"repin_count"`
	FavoriteCount *flexInt `json:"favorite_count"`
}

func decodeZhihuPin(data []byte, t Target) (*post, error) {
	var pin zhihuPin
	if err := json.Unmarshal(data, &pin); err != nil {
		return nil, err
	}
	if err := zhihuCheck(pin.ID, t, "pin/"); err != nil {
		return nil, err
	}
	p := &post{
		ID:        string(pin.ID),
		URL:       "https://www.zhihu.com/pin/" + string(pin.ID),
		CreatedAt: unixRFC3339(pin.Created),
		Author:    pin.Author.author(),
		Stats: &postStats{Likes: optCount(pin.LikeCount), Comments: optCount(pin.CommentCount),
			Shares: optCount(pin.RepinCount), Collects: optCount(pin.FavoriteCount)},
	}
	var texts []string
	for _, c := range pin.Content {
		switch c.Type {
		case "text":
			texts = append(texts, htmlText(c.Content))
		case "image":
			if u := nonEmpty(c.OriginalURL, c.URL); u != "" {
				p.Images = append(p.Images, postImage{URL: httpsURL(u), Width: c.Width, Height: c.Height})
			}
		}
	}
	p.Text = strings.Join(texts, "\n")
	return p, nil
}

type zhihuQuestion struct {
	ID            flexString `json:"id"`
	Title         string     `json:"title"`
	Detail        string     `json:"detail"`
	Created       int64      `json:"created"`
	AnswerCount   *flexInt   `json:"answer_count"`
	VisitCount    *flexInt   `json:"visit_count"`
	FollowerCount *flexInt   `json:"follower_count"`
}

// 问题详情不带提问者（实测），作者留空。
func decodeZhihuQuestion(data []byte, t Target) (*post, error) {
	var q zhihuQuestion
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	if err := zhihuCheck(q.ID, t, "question/"); err != nil {
		return nil, err
	}
	return &post{
		ID:        string(q.ID),
		URL:       "https://www.zhihu.com/question/" + string(q.ID),
		Kind:      "text",
		Title:     q.Title,
		Text:      htmlText(q.Detail),
		CreatedAt: unixRFC3339(q.Created),
		Images:    htmlImages(q.Detail),
		Stats:     &postStats{Comments: optCount(q.AnswerCount), Views: optCount(q.VisitCount), Collects: optCount(q.FollowerCount)},
	}, nil
}

// ======================== Threads ========================

// threadsCaption：v1 给 {"text": …}，v2 直接给字符串（实测）。
type threadsCaption string

func (c *threadsCaption) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = threadsCaption(s)
		return nil
	}
	var o struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(b, &o)
	*c = threadsCaption(o.Text)
	return nil
}

// threadsPost 是 Instagram 私有 API 的形状（image_versions2 / video_versions / carousel_media）。
type threadsPost struct {
	igMedia
	TakenAt        int64          `json:"taken_at"`
	Caption        threadsCaption `json:"caption"`
	OriginalWidth  int            `json:"original_width"`
	OriginalHeight int            `json:"original_height"`
	User           struct {
		PK            flexString `json:"pk"`
		Username      string     `json:"username"`
		FullName      string     `json:"full_name"`
		ProfilePicURL string     `json:"profile_pic_url"`
	} `json:"user"`
	CarouselMedia []igMedia `json:"carousel_media"`
	LikeCount     *flexInt  `json:"like_count"`
	// 回复与转发数：v2 在帖子上，v1 在 text_post_app_info 里
	DirectReplyCount *flexInt `json:"direct_reply_count"`
	RepostCount      *flexInt `json:"repost_count"`
	TextPostAppInfo  struct {
		DirectReplyCount *flexInt `json:"direct_reply_count"`
		RepostCount      *flexInt `json:"repost_count"`
	} `json:"text_post_app_info"`
}

type threadsV1Envelope struct {
	Node struct {
		ThreadItems []struct {
			Post *threadsPost `json:"post"`
		} `json:"thread_items"`
	} `json:"node"`
}

type threadsV2Envelope struct {
	Post *threadsPost `json:"post"`
}

func decodeThreadsV1(data []byte, t Target) (*post, error) {
	var env threadsV1Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	item, ok := first(env.Node.ThreadItems)
	if !ok || item.Post == nil {
		return nil, errors.New("没有 thread_items")
	}
	return threadsToPost(*item.Post, t)
}

func decodeThreadsV2(data []byte, t Target) (*post, error) {
	var env threadsV2Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Post == nil {
		return nil, errors.New("没有 post")
	}
	return threadsToPost(*env.Post, t)
}

func threadsToPost(m threadsPost, t Target) (*post, error) {
	if m.Code != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, m.Code)
	}
	u := m.User
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://www.threads.com/@" + nonEmpty(u.Username, "_") + "/post/" + t.ResourceID,
		Text:      string(m.Caption),
		CreatedAt: unixRFC3339(m.TakenAt),
		Author: &postAuthor{Name: nonEmpty(u.FullName, u.Username), Handle: atHandle(u.Username), ID: string(u.PK),
			Avatar: u.ProfilePicURL, URL: profileURL("https://www.threads.com/@", u.Username)},
		Stats: &postStats{Likes: optCount(m.LikeCount),
			Comments: optCount(firstNonNil(m.DirectReplyCount, m.TextPostAppInfo.DirectReplyCount)),
			Shares:   optCount(firstNonNil(m.RepostCount, m.TextPostAppInfo.RepostCount))},
	}
	items := []igMedia{m.igMedia}
	if len(m.CarouselMedia) > 0 {
		items = m.CarouselMedia
	}
	for i, it := range items {
		img := it.image()
		w, h := img.Width, img.Height
		if i == 0 && len(m.CarouselMedia) == 0 && it.MediaType == 2 && m.OriginalWidth > 0 {
			w, h = m.OriginalWidth, m.OriginalHeight // 视频版本不带尺寸，取帖子的原始尺寸
		}
		igAdd(p, it.MediaType == 2, img.URL, it.video(), w, h, int64(it.VideoDuration*1000))
	}
	return p, nil
}

func firstNonNil[T any](xs ...*T) *T {
	for _, x := range xs {
		if x != nil {
			return x
		}
	}
	return nil
}

// ======================== Reddit ========================

type redditMediaSource struct {
	URL        string `json:"url"`
	Dimensions struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"dimensions"`
}

type redditPost struct {
	ID        string `json:"id"` // t3_<id>
	CreatedAt string `json:"createdAt"`
	PostTitle string `json:"postTitle"`
	URL       string `json:"url"` // 图片帖是 i.redd.it 原图，链接帖是外链
	Permalink string `json:"permalink"`
	Domain    string `json:"domain"`
	PostHint  string `json:"postHint"` // IMAGE | HOSTED_VIDEO | …
	Content   *struct {
		Markdown string `json:"markdown"`
	} `json:"content"`
	Score        *flexInt `json:"score"`
	CommentCount *flexInt `json:"commentCount"`
	PostStats    struct {
		ShareAllTotal  *flexInt `json:"shareAllTotal"`
		ViewCountTotal *flexInt `json:"viewCountTotal"`
	} `json:"postStats"`
	AuthorInfo *struct {
		Name      string             `json:"name"`
		ID        string             `json:"id"`
		IconSmall *redditMediaSource `json:"iconSmall"`
	} `json:"authorInfo"`
	Thumbnail *redditMediaSource `json:"thumbnail"`
	Media     *struct {
		Still *struct {
			Source *redditMediaSource `json:"source"`
		} `json:"still"`
		Streaming *struct {
			Dimensions struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"dimensions"`
			Duration float64 `json:"duration"` // 秒
		} `json:"streaming"`
		// 带音轨的合成 mp4，键是 low / medium / high / highest / recommended。
		// streaming 里的 DASH 是音视频分开的，下载下来没有声音。
		PackagedMedia *struct {
			MuxedMp4s map[string]struct {
				URL string `json:"url"`
			} `json:"muxedMp4s"`
		} `json:"packagedMedia"`
	} `json:"media"`
}

type redditEnvelope struct {
	Posts []redditPost `json:"postsInfoByIds"`
}

// redditTime 解析 "2025-10-30T01:58:59.142000+0000"。
func redditTime(s string) string {
	t, err := time.Parse("2006-01-02T15:04:05.999999-0700", s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// redditHosted 判断帖子的 url 是不是 Reddit 自己托管的东西（图片、视频、帖子本身），
// 不是的话就是链接帖的外链，要写进正文，否则用户收藏的那个链接就丢了。
func redditHosted(domain string) bool {
	return strings.HasPrefix(domain, "self.") || strings.HasSuffix(domain, "redd.it") || strings.HasSuffix(domain, "reddit.com")
}

func decodeReddit(data []byte, t Target) (*post, error) {
	var env redditEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	r, ok := first(env.Posts)
	if !ok {
		return nil, errors.New("没有 postsInfoByIds")
	}
	if r.ID != "t3_"+t.ResourceID {
		return nil, fmt.Errorf("%w：要 t3_%s，拿到 %s", errWrongItem, t.ResourceID, r.ID)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://www.reddit.com" + r.Permalink,
		Title:     r.PostTitle,
		CreatedAt: redditTime(r.CreatedAt),
		Stats: &postStats{Likes: optCount(r.Score), Comments: optCount(r.CommentCount),
			Shares: optCount(r.PostStats.ShareAllTotal), Views: optCount(r.PostStats.ViewCountTotal)},
	}
	if r.Content != nil {
		p.Text = r.Content.Markdown
	}
	if r.URL != "" && !redditHosted(r.Domain) {
		p.Text = strings.TrimSpace(p.Text + "\n\n" + r.URL)
	}
	if a := r.AuthorInfo; a != nil && a.Name != "" {
		p.Author = &postAuthor{Name: a.Name, Handle: "u/" + a.Name, ID: a.ID, URL: "https://www.reddit.com/user/" + a.Name}
		if a.IconSmall != nil {
			p.Author.Avatar = a.IconSmall.URL
		}
	}
	var still *redditMediaSource
	if m := r.Media; m != nil && m.Still != nil {
		still = m.Still.Source
	}
	switch m := r.Media; {
	case m != nil && m.PackagedMedia != nil:
		for _, q := range []string{"highest", "high", "medium", "recommended", "low"} {
			if v := m.PackagedMedia.MuxedMp4s[q]; v.URL != "" {
				p.Video = &postVideo{URL: v.URL}
				break
			}
		}
		if p.Video != nil && m.Streaming != nil {
			p.Video.Width, p.Video.Height = m.Streaming.Dimensions.Width, m.Streaming.Dimensions.Height
			p.Video.DurationMs = int64(m.Streaming.Duration * 1000)
		}
		if still != nil {
			p.Cover = still.URL
		}
	case r.PostHint == "IMAGE" && strings.HasPrefix(r.URL, "https://i.redd.it/"):
		// url 是原图；still.source 是同尺寸的 webp 转码
		img := postImage{URL: r.URL}
		if still != nil {
			img.Width, img.Height = still.Dimensions.Width, still.Dimensions.Height
		}
		p.Images = []postImage{img}
	case still != nil:
		p.Images = []postImage{{URL: still.URL, Width: still.Dimensions.Width, Height: still.Dimensions.Height}}
	case r.Thumbnail != nil:
		p.Cover = r.Thumbnail.URL
	}
	return p, nil
}

// ======================== 视频号 ========================

type wxChannelsVideo struct {
	ID           flexString `json:"id"`
	Username     string     `json:"username"`
	Nickname     string     `json:"nickname"`
	Description  string     `json:"description"`
	CreateTime   int64      `json:"create_time"`
	LikeCount    *flexInt   `json:"like_count"`
	FavCount     *flexInt   `json:"fav_count"`
	ForwardCount *flexInt   `json:"forward_count"`
	CommentCount *flexInt   `json:"comment_count"`
	Media        *struct {
		URL           string `json:"url"`
		URLToken      string `json:"url_token"` // 同封面：视频地址拼上它才能下载
		DecodeKey     string `json:"decode_key"`
		CoverURL      string `json:"cover_url"`
		CoverURLToken string `json:"cover_url_token"` // 封面地址要拼上它才能下载（不拼返回 400，实测）
		Duration      int64  `json:"duration"`        // 秒
		Width         int    `json:"width"`
		Height        int    `json:"height"`
	} `json:"media"`
}

// wxChannelsTopSpec 是视频号转码档里最清晰的一档。地址本身给的是默认档（url_token 里 basedata 指定，
// 实测 xWT113 = 576×1024、0.5 Mbps），带 X-snsvideoflag 才换档：xWT112 次之，xWT111 是 1080p。
// 作品没有这一档时 CDN 照给默认档（实测不存在的档位名都退回默认档），不会失败。
// 原片（file_size 那份，去掉 basedata 才给）只能取一次、再取 400，不用（2026-10-25 实测）。
const wxChannelsTopSpec = "xWT111"

// 视频号的 title 与 description 是同一段文字（实测），不是标题，只进正文。
// 视频文件是加密的：开头 131072 字节与 ISAAC64(decode_key) 的密钥流异或过，其余是明文。
// 服务端不经手文件，地址与密钥一起交给客户端，客户端下载后解开再上传（2026-10-25 起；
// 原先客户端不会解密，只给封面）。不会解密的客户端拿不到这个视频，见 forClient。
func decodeWechatChannels(data []byte, t Target) (*post, error) {
	var v wxChannelsVideo
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	if v.ID == "" || v.Media == nil {
		return nil, errors.New("没有作品")
	}
	m := v.Media
	video := &postVideo{DurationMs: m.Duration * 1000, Width: m.Width, Height: m.Height}
	if m.URL != "" && m.URLToken != "" {
		video.URL = httpsURL(m.URL+m.URLToken) + "&X-snsvideoflag=" + wxChannelsTopSpec
		video.DecodeKey = m.DecodeKey
	}
	return &post{
		ID:        t.ResourceID,
		URL:       "https://weixin.qq.com/sph/" + t.ResourceID,
		Kind:      "video",
		Text:      v.Description,
		CreatedAt: unixRFC3339(v.CreateTime),
		Author:    &postAuthor{Name: v.Nickname, ID: v.Username},
		Cover:     httpsURL(m.CoverURL + m.CoverURLToken),
		Video:     video,
		Stats: &postStats{Likes: optCount(v.LikeCount), Comments: optCount(v.CommentCount),
			Shares: optCount(v.ForwardCount), Collects: optCount(v.FavCount)},
	}, nil
}

// ======================== 头条 / 西瓜视频 ========================

// toutiaoKind 从链接判断内容类型："video"、"article"，看不出来（/group/、/item/、/i…）时为空，
// 那时文章接口先试，发现是视频（video_count > 0）就交给视频接口。
func toutiaoKind(resolved string) string {
	u, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	switch {
	case strings.HasSuffix(u.Host, "ixigua.com"), strings.HasPrefix(u.Path, "/video/"):
		return "video"
	case strings.HasPrefix(u.Path, "/article/"), strings.HasPrefix(u.Path, "/w/"), strings.HasPrefix(u.Path, "/a"):
		return "article"
	}
	return ""
}

type toutiaoImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type toutiaoArticle struct {
	GroupID         flexString     `json:"group_id"`
	Content         string         `json:"content"`
	VideoCount      int            `json:"video_count"`
	MediaUserID     flexString     `json:"media_user_id"`
	OriginImageList []toutiaoImage `json:"origin_image_list"` // 原图，与正文里的图一一对应
	H5Extra         struct {
		Title        string     `json:"title"`
		Source       string     `json:"source"`
		PublishStamp flexString `json:"publish_stamp"`
		Media        struct {
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		} `json:"media"`
	} `json:"h5_extra"`
}

type toutiaoArticleEnvelope struct {
	Data *toutiaoArticle `json:"data"`
}

var toutiaoArticleBody = regexp.MustCompile(`(?s)<article>(.*)</article>`)

// 文章正文是 HTML（标题栏在 <header>，正文在 <article>）；微头条的 <article> 里装的是 JSON
// {"rich_content": {"text": …}, "images": […]}，它没有标题 —— h5_extra.title 只是正文的开头，不当标题用。
func decodeToutiaoArticle(data []byte, t Target) (*post, error) {
	var env toutiaoArticleEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	a := env.Data
	if a == nil {
		return nil, errors.New("没有 data")
	}
	if string(a.GroupID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, a.GroupID)
	}
	if a.VideoCount > 0 {
		return nil, fmt.Errorf("%w：这是视频", errOtherKind)
	}
	ts, _ := strconv.ParseInt(string(a.H5Extra.PublishStamp), 10, 64)
	p := &post{
		ID:        t.ResourceID,
		CreatedAt: unixRFC3339(ts),
		Author: &postAuthor{Name: nonEmpty(a.H5Extra.Media.Name, a.H5Extra.Source), ID: string(a.MediaUserID),
			Avatar: a.H5Extra.Media.AvatarURL},
	}
	for _, img := range a.OriginImageList {
		if img.URL != "" {
			p.Images = append(p.Images, postImage{URL: img.URL, Width: img.Width, Height: img.Height})
		}
	}
	body := ""
	if m := toutiaoArticleBody.FindStringSubmatch(a.Content); m != nil {
		body = strings.TrimSpace(m[1])
	}
	var rich struct {
		RichContent struct {
			Text string `json:"text"`
		} `json:"rich_content"`
	}
	if strings.HasPrefix(body, "{") && json.Unmarshal([]byte(body), &rich) == nil {
		p.URL = "https://www.toutiao.com/w/" + t.ResourceID + "/"
		p.Text = rich.RichContent.Text
		return p, nil
	}
	p.URL = "https://www.toutiao.com/article/" + t.ResourceID + "/"
	p.Kind = "article"
	p.Title = a.H5Extra.Title
	p.Text = htmlText(body)
	return p, nil
}

// toutiaoPlayInfo 是 video_play_info：一段 JSON 字符串。video_list 网页版是对象（video_1…）、
// main_url 是 base64；App 版是数组、main_url 是明文、清晰度信息在 video_meta 里（实测）。
type toutiaoPlayInfo struct {
	Duration float64 `json:"video_duration"` // 秒
	Poster   string  `json:"poster_url"`
	best     streamPick
}

func (pi *toutiaoPlayInfo) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil || s == "" {
		return nil
	}
	var raw struct {
		Duration  float64         `json:"video_duration"`
		Poster    string          `json:"poster_url"`
		VideoList json.RawMessage `json:"video_list"`
	}
	if json.Unmarshal([]byte(s), &raw) != nil {
		return nil
	}
	pi.Duration, pi.Poster = raw.Duration, raw.Poster
	type item struct {
		MainURL   string  `json:"main_url"`
		VWidth    flexInt `json:"vwidth"`
		VHeight   flexInt `json:"vheight"`
		Bitrate   flexInt `json:"bitrate"`
		CodecType string  `json:"codec_type"` // 网页版：h264 / bytevc1 / …
		Meta      *struct {
			VWidth    flexInt `json:"vwidth"`
			VHeight   flexInt `json:"vheight"`
			Bitrate   flexInt `json:"bitrate"`
			CodecType string  `json:"codec_type"`
		} `json:"video_meta"`
	}
	var items []item
	if json.Unmarshal(raw.VideoList, &items) != nil {
		var byKey map[string]item
		_ = json.Unmarshal(raw.VideoList, &byKey)
		for _, it := range byKey {
			items = append(items, it)
		}
	}
	for _, it := range items {
		u := it.MainURL
		if !strings.HasPrefix(u, "http") {
			dec, err := base64.StdEncoding.DecodeString(u)
			if err != nil {
				continue
			}
			u = string(dec)
		}
		w, h, rate, codec := int(it.VWidth), int(it.VHeight), int64(it.Bitrate), it.CodecType
		if it.Meta != nil {
			w, h, rate, codec = int(it.Meta.VWidth), int(it.Meta.VHeight), int64(it.Meta.Bitrate), it.Meta.CodecType
		}
		if strings.HasPrefix(u, "http") {
			pi.best.offer(u, w, h, rate, codec)
		}
	}
	return nil
}

func (pi toutiaoPlayInfo) video() *postVideo {
	if pi.best.URL == "" {
		return nil
	}
	return &postVideo{URL: pi.best.URL, Width: pi.best.W, Height: pi.best.H, DurationMs: int64(pi.Duration * 1000)}
}

type toutiaoCounter struct {
	CommentCount    *flexInt `json:"commentCount"`
	DiggCount       *flexInt `json:"diggCount"`
	ShareCount      *flexInt `json:"shareCount"`
	RepinCount      *flexInt `json:"repinCount"`
	VideoWatchCount *flexInt `json:"videoWatchCount"`
}

func (c toutiaoCounter) stats() *postStats {
	return &postStats{Likes: optCount(c.DiggCount), Comments: optCount(c.CommentCount), Shares: optCount(c.ShareCount),
		Collects: optCount(c.RepinCount), Views: optCount(c.VideoWatchCount)}
}

type toutiaoWebVideo struct {
	GroupID  flexString `json:"group_id"`
	ItemCell struct {
		UserInfo struct {
			UserID    flexString `json:"userID"`
			Name      string     `json:"name"`
			AvatarURL string     `json:"avatarURL"`
		} `json:"userInfo"`
		ItemCounter toutiaoCounter `json:"itemCounter"`
		ShareInfo   struct {
			Title string `json:"title"`
		} `json:"shareInfo"`
	} `json:"itemCell"`
	VideoPlayInfo toutiaoPlayInfo `json:"video_play_info"`
}

type toutiaoWebVideoEnvelope struct {
	Data *toutiaoWebVideo `json:"data"`
}

// 网页版视频接口没有标题字段，只有分享标题（「…  - 今日头条」），去掉后缀就是视频标题。
func decodeToutiaoWebVideo(data []byte, t Target) (*post, error) {
	var env toutiaoWebVideoEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	v := env.Data
	if v == nil {
		return nil, errors.New("没有 data")
	}
	if string(v.GroupID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, v.GroupID)
	}
	u := v.ItemCell.UserInfo
	return &post{
		ID:     t.ResourceID,
		URL:    "https://www.toutiao.com/video/" + t.ResourceID + "/",
		Kind:   "video",
		Title:  strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v.ItemCell.ShareInfo.Title), "- 今日头条")),
		Author: &postAuthor{Name: u.Name, ID: string(u.UserID), Avatar: u.AvatarURL},
		Cover:  v.VideoPlayInfo.Poster,
		Video:  v.VideoPlayInfo.video(),
		Stats:  v.ItemCell.ItemCounter.stats(),
	}, nil
}

// toutiaoAppVideo 有两种形态（实测）：
//   - 长视频（西瓜视频那一类）：user_info、itemCell.itemCounter、video_play_info；
//   - 短视频（新发的竖屏视频）：user.info、action、video.play_addr_list，没有 video_play_info。
type toutiaoAppVideo struct {
	GroupID     flexString `json:"group_id"`
	Title       string     `json:"title"`
	Abstract    string     `json:"abstract"`
	PublishTime int64      `json:"publish_time"`
	UserInfo    *struct {
		UserID    flexString `json:"user_id"`
		Name      string     `json:"name"`
		AvatarURL string     `json:"avatar_url"`
	} `json:"user_info"`
	ItemCell *struct {
		ItemCounter toutiaoCounter `json:"itemCounter"`
	} `json:"itemCell"`
	VideoDetailInfo *struct {
		LargeImage toutiaoImage `json:"detail_video_large_image"`
	} `json:"video_detail_info"`
	VideoPlayInfo toutiaoPlayInfo `json:"video_play_info"`

	// 短视频形态
	User *struct {
		Info struct {
			UserID    flexString `json:"user_id"`
			Name      string     `json:"name"`
			AvatarURL string     `json:"avatar_url"`
		} `json:"info"`
	} `json:"user"`
	Action *struct {
		CommentCount *flexInt `json:"comment_count"`
		DiggCount    *flexInt `json:"digg_count"`
		ShareCount   *flexInt `json:"share_count"`
		RepinCount   *flexInt `json:"repin_count"`
		PlayCount    *flexInt `json:"play_count"`
	} `json:"action"`
	LargeImageList []toutiaoImage    `json:"large_image_list"`
	Video          *toutiaoShortClip `json:"video"`
}

type toutiaoShortClip struct {
	Width        int     `json:"width"` // play_addr 那一档的尺寸；play_addr_list 各档只给清晰度名
	Height       int     `json:"height"`
	Duration     float64 `json:"duration"` // 秒
	PlayAddrList []struct {
		PlayURLList []string `json:"play_url_list"`
		Definition  string   `json:"definition"` // 1080p / 720p / …
		Bitrate     flexInt  `json:"bitrate"`
		CodecType   string   `json:"codec_type"` // bytevc1 / …
	} `json:"play_addr_list"`
}

// best 先看编码档（codecRank），同档按清晰度名取最高档；尺寸按 play_addr 那一档的比例换算
// （720×1280 的 1080p 是 1080×1920）。
func (c *toutiaoShortClip) best() *postVideo {
	if c == nil {
		return nil
	}
	short := min(c.Width, c.Height)
	var v *postVideo
	var bestDef, bestRank int
	var bestRate int64
	for _, a := range c.PlayAddrList {
		u, ok := first(a.PlayURLList)
		def, _ := strconv.Atoi(strings.TrimSuffix(a.Definition, "p"))
		rank := codecRank(a.CodecType)
		if !ok || u == "" || (v != nil && (rank > bestRank ||
			rank == bestRank && (def < bestDef || def == bestDef && int64(a.Bitrate) <= bestRate))) {
			continue
		}
		bestDef, bestRate, bestRank = def, int64(a.Bitrate), rank
		v = &postVideo{URL: u, DurationMs: int64(c.Duration * 1000)}
		if short > 0 && def > 0 {
			v.Width, v.Height = c.Width*def/short, c.Height*def/short
		}
	}
	return v
}

type toutiaoAppVideoEnvelope struct {
	ErrNo   int              `json:"err_no"`
	ErrTips string           `json:"err_tips"`
	Data    *toutiaoAppVideo `json:"data"`
}

func decodeToutiaoAppVideo(data []byte, t Target) (*post, error) {
	var env toutiaoAppVideoEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	v := env.Data
	if env.ErrNo != 0 || v == nil {
		return nil, fmt.Errorf("头条返回 err_no=%d %s", env.ErrNo, env.ErrTips)
	}
	if string(v.GroupID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, v.GroupID)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://www.toutiao.com/video/" + t.ResourceID + "/",
		Kind:      "video",
		Title:     v.Title,
		Text:      v.Abstract,
		CreatedAt: unixRFC3339(v.PublishTime),
		Cover:     v.VideoPlayInfo.Poster,
		Video:     v.VideoPlayInfo.video(),
	}
	switch {
	case v.UserInfo != nil:
		p.Author = &postAuthor{Name: v.UserInfo.Name, ID: string(v.UserInfo.UserID), Avatar: v.UserInfo.AvatarURL}
	case v.User != nil:
		p.Author = &postAuthor{Name: v.User.Info.Name, ID: string(v.User.Info.UserID), Avatar: v.User.Info.AvatarURL}
	}
	switch {
	case v.ItemCell != nil:
		p.Stats = v.ItemCell.ItemCounter.stats()
	case v.Action != nil:
		p.Stats = &postStats{Likes: optCount(v.Action.DiggCount), Comments: optCount(v.Action.CommentCount),
			Shares: optCount(v.Action.ShareCount), Collects: optCount(v.Action.RepinCount), Views: optCount(v.Action.PlayCount)}
	}
	if p.Video == nil {
		p.Video = v.Video.best()
	}
	if p.Cover == "" && v.VideoDetailInfo != nil {
		p.Cover = v.VideoDetailInfo.LargeImage.URL
	}
	if img, ok := first(v.LargeImageList); ok && p.Cover == "" {
		p.Cover = img.URL
	}
	return p, nil
}

// ======================== Lemon8 ========================

type lemon8URL struct {
	URL string `json:"url"`
}

type lemon8Image struct {
	URL         string      `json:"url"`
	URLList     []lemon8URL `json:"url_list"`
	URLWithLogo string      `json:"url_with_logo"`
	Width       int         `json:"width"`
	Height      int         `json:"height"`
}

// pick 选一张客户端能显示的图，没有时为空。图文作品的无水印图只有 HEIC（1080 宽 q90，实测换扩展名
// 签名失效），Android、网页与桌面端都解不了 HEIC（见客户端 thumbnailer），选它等于给用户一张看不了的图；
// 有非 HEIC 的就用，没有就退到带作者水印的 webp。
func (im lemon8Image) pick() string {
	cands := []string{im.URL}
	for _, u := range im.URLList {
		cands = append(cands, u.URL)
	}
	cands = append(cands, im.URLWithLogo)
	for _, u := range cands {
		if u != "" && !isHEIC(u) {
			return u
		}
	}
	return ""
}

func isHEIC(u string) bool {
	path, _, _ := strings.Cut(u, "?")
	return strings.HasSuffix(path, ".heic") || strings.HasSuffix(path, ".heif")
}

// lemon8VideoModel 是 video.video_model：一段 JSON 字符串，video_list 每档的清晰度在 video_meta 里。
type lemon8VideoModel struct {
	best streamPick
}

func (m *lemon8VideoModel) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil || s == "" {
		return nil
	}
	var raw struct {
		VideoList []struct {
			MainURL string `json:"main_url"`
			Meta    struct {
				VWidth    flexInt `json:"vwidth"`
				VHeight   flexInt `json:"vheight"`
				Bitrate   flexInt `json:"bitrate"`
				CodecType string  `json:"codec_type"` // h264 / h265 / …
			} `json:"video_meta"`
		} `json:"video_list"`
	}
	_ = json.Unmarshal([]byte(s), &raw)
	for _, it := range raw.VideoList {
		if strings.HasPrefix(it.MainURL, "http") {
			m.best.offer(it.MainURL, int(it.Meta.VWidth), int(it.Meta.VHeight), int64(it.Meta.Bitrate), it.Meta.CodecType)
		}
	}
	return nil
}

type lemon8Post struct {
	ItemID      flexString `json:"item_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	PublishTime int64      `json:"publish_time"`
	Author      struct {
		Name       string     `json:"name"`
		AuthorID   flexString `json:"author_id"`
		UniqueName string     `json:"user_unique_name"`
		Avatar     struct {
			URLList []lemon8URL `json:"url_list"`
		} `json:"avatar"`
	} `json:"author"`
	LikeCount     *flexInt      `json:"like_count"`
	CommentCount  *flexInt      `json:"comment_count"`
	FavoriteCount *flexInt      `json:"favorite_count"`
	ImageList     []lemon8Image `json:"image_list"`
	OriginImage   *lemon8Image  `json:"origin_image"`
	Video         *struct {
		Duration        float64          `json:"duration"` // 秒
		VideoModel      lemon8VideoModel `json:"video_model"`
		FirstFrameImage lemon8Image      `json:"first_frame_image"`
	} `json:"video"`
}

type lemon8Envelope struct {
	Data *lemon8Post `json:"data"`
}

func decodeLemon8(data []byte, t Target) (*post, error) {
	var env lemon8Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	l := env.Data
	if l == nil {
		return nil, errors.New("没有 data") // 失败时是 {"message": "error", "error_message": "InternalErr"}，照样计费
	}
	if string(l.ItemID) != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, l.ItemID)
	}
	a := l.Author
	avatar, _ := first(a.Avatar.URLList)
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://www.lemon8-app.com/@" + nonEmpty(a.UniqueName, "_") + "/" + t.ResourceID,
		Title:     l.Title,
		Text:      l.Description,
		CreatedAt: unixRFC3339(l.PublishTime),
		Author: &postAuthor{Name: a.Name, Handle: atHandle(a.UniqueName), ID: string(a.AuthorID), Avatar: avatar.URL,
			URL: profileURL("https://www.lemon8-app.com/@", a.UniqueName)},
		Stats: &postStats{Likes: optCount(l.LikeCount), Comments: optCount(l.CommentCount), Collects: optCount(l.FavoriteCount)},
	}
	if p.Text == p.Title { // 视频作品的标题与描述是同一段（实测）
		p.Text = ""
	}
	for _, im := range l.ImageList {
		if u := im.pick(); u != "" {
			p.Images = append(p.Images, postImage{URL: u, Width: im.Width, Height: im.Height})
		}
	}
	if v := l.Video; v != nil && v.VideoModel.best.URL != "" && len(p.Images) == 0 {
		b := v.VideoModel.best
		p.Video = &postVideo{URL: b.URL, Width: b.W, Height: b.H, DurationMs: int64(v.Duration * 1000)}
		// 首帧图也只有 HEIC（实测），退到 origin_image（webp 原图）
		p.Cover = v.FirstFrameImage.pick()
		if p.Cover == "" && l.OriginImage != nil {
			p.Cover = l.OriginImage.pick()
		}
	}
	return p, nil
}

// ======================== 皮皮虾 ========================

type ppxURLs struct {
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	CodecType *flexInt `json:"codec_type"` // 取值同抖音的 is_bytevc1，见 bytevcCodec
	URLList   []struct {
		URL string `json:"url"`
	} `json:"url_list"`
}

func (u *ppxURLs) first() string {
	if u == nil || len(u.URLList) == 0 {
		return ""
	}
	return u.URLList[0].URL
}

type ppxItem struct {
	ItemIDStr  string  `json:"item_id_str"`
	Content    string  `json:"content"`
	CreateTime int64   `json:"create_time"`
	Duration   float64 `json:"duration"` // 秒
	Cover      ppxURLs `json:"cover"`
	Author     struct {
		IDStr  string  `json:"id_str"`
		Name   string  `json:"name"`
		Avatar ppxURLs `json:"avatar"`
	} `json:"author"`
	Stats struct {
		LikeCount     *flexInt `json:"like_count"`
		CommentCount  *flexInt `json:"comment_count"`
		ShareCount    *flexInt `json:"share_count"`
		PlayCount     *flexInt `json:"play_count"`
		FavoriteCount *flexInt `json:"favorite_count"`
	} `json:"stats"`
	// origin_video_download 是原片（实测码率最高）；video 下各档依次降级
	OriginVideoDownload *ppxURLs `json:"origin_video_download"`
	Video               *struct {
		VideoDownload *ppxURLs `json:"video_download"`
		VideoHigh     *ppxURLs `json:"video_high"`
		VideoMid      *ppxURLs `json:"video_mid"`
		VideoLow      *ppxURLs `json:"video_low"`
	} `json:"video"`
}

type ppxEnvelope struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
	Data       struct {
		Data *struct {
			Item *ppxItem `json:"item"`
		} `json:"data"`
	} `json:"data"`
}

// 只按视频作品写（实测样本是视频；话题流里取样的 12 条也全是视频），图文作品只有封面与文字。
func decodePipixia(data []byte, t Target) (*post, error) {
	var env ppxEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.StatusCode != 0 || env.Data.Data == nil || env.Data.Data.Item == nil {
		return nil, fmt.Errorf("皮皮虾返回 status_code=%d %s", env.StatusCode, env.Message)
	}
	it := env.Data.Data.Item
	if it.ItemIDStr != t.ResourceID {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, t.ResourceID, it.ItemIDStr)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://h5.pipix.com/item/" + t.ResourceID,
		Text:      it.Content,
		CreatedAt: unixRFC3339(it.CreateTime),
		Author:    &postAuthor{Name: it.Author.Name, ID: it.Author.IDStr, Avatar: it.Author.Avatar.first()},
		Cover:     it.Cover.first(),
		Stats: &postStats{Likes: optCount(it.Stats.LikeCount), Comments: optCount(it.Stats.CommentCount),
			Shares: optCount(it.Stats.ShareCount), Views: optCount(it.Stats.PlayCount), Collects: optCount(it.Stats.FavoriteCount)},
	}
	cands := []*ppxURLs{it.OriginVideoDownload}
	if v := it.Video; v != nil {
		cands = append(cands, v.VideoDownload, v.VideoHigh, v.VideoMid, v.VideoLow)
	}
	// 同分辨率时先出现的胜（offer 只在严格更好时替换），原片排在最前
	var pick streamPick
	for _, c := range cands {
		if c == nil {
			continue
		}
		codec := ""
		if c.CodecType != nil {
			codec = bytevcCodec(int(*c.CodecType))
		}
		pick.offer(c.first(), c.Width, c.Height, 0, codec)
	}
	if pick.URL != "" {
		p.Video = &postVideo{URL: pick.URL, Width: pick.W, Height: pick.H, DurationMs: int64(it.Duration * 1000)}
	}
	return p, nil
}

// ======================== LinkedIn ========================

// linkedinImage：images 字段在实测样本里都是 null，元素形态没见过，字符串与 {"url"} 两种都接。
type linkedinImage string

func (l *linkedinImage) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = linkedinImage(s)
		return nil
	}
	var o struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(b, &o)
	*l = linkedinImage(o.URL)
	return nil
}

type linkedinPost struct {
	ID             flexString      `json:"id"`
	Error          string          `json:"error"` // 超时等失败也是 HTTP 200（计费），只在这里写原因
	UserID         string          `json:"user_id"`
	UserName       string          `json:"user_name"`
	AuthorPic      string          `json:"author_profile_pic"`
	Headline       string          `json:"headline"`
	PostText       string          `json:"post_text"`
	DatePosted     string          `json:"date_posted"`
	Images         []linkedinImage `json:"images"`
	Videos         []string        `json:"videos"` // 混着 embed 页面地址，只取 mp4
	VideoThumbnail string          `json:"video_thumbnail"`
	VideoDuration  flexInt         `json:"video_duration"` // 秒
	NumLikes       *flexInt        `json:"num_likes"`
	NumComments    *flexInt        `json:"num_comments"`
}

var linkedinVideoHeight = regexp.MustCompile(`/mp4-(\d+)p`)

func decodeLinkedIn(data []byte, t Target) (*post, error) {
	var l linkedinPost
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	if l.Error != "" {
		return nil, fmt.Errorf("LinkedIn 接口失败：%s", l.Error)
	}
	if id, ok := strings.CutPrefix(t.ResourceID, "activity/"); ok && string(l.ID) != id {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s", errWrongItem, id, l.ID)
	}
	created := ""
	if ts, err := time.Parse(time.RFC3339Nano, l.DatePosted); err == nil {
		created = ts.UTC().Format(time.RFC3339)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       canonicalURL(t.Resolved),
		Text:      l.PostText,
		CreatedAt: created,
		Author: &postAuthor{Name: l.UserName, Handle: l.UserID, Avatar: l.AuthorPic,
			URL: profileURL("https://www.linkedin.com/in/", l.UserID)},
		Stats: &postStats{Likes: optCount(l.NumLikes), Comments: optCount(l.NumComments)},
	}
	if strings.HasPrefix(t.ResourceID, "pulse/") {
		p.Kind, p.Title = "article", l.Headline // 文章才有真正的标题；帖子的 headline 只是正文第一句
	}
	for _, im := range l.Images {
		if strings.HasPrefix(string(im), "http") {
			p.Images = append(p.Images, postImage{URL: string(im)})
		}
	}
	bestH := -1
	for _, v := range l.Videos {
		m := linkedinVideoHeight.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		if h, _ := strconv.Atoi(m[1]); h > bestH {
			bestH = h
			p.Video = &postVideo{URL: v, DurationMs: int64(l.VideoDuration) * 1000}
		}
	}
	if p.Video != nil {
		p.Cover = l.VideoThumbnail
	}
	return p, nil
}

// canonicalURL 去掉查询串与片段（分享追踪参数）。
func canonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// ======================== Telegram 公开频道 ========================

type tgPost struct {
	ID      flexString  `json:"id"`
	Channel string      `json:"channel"`
	Date    string      `json:"date"`
	Text    string      `json:"text"`
	Views   abbrevCount `json:"views"`  // 只给缩写（「3.83M」），换算成近似整数
	Author  string      `json:"author"` // 频道名称
	Media   struct {
		Photos []string `json:"photos"`
		Videos []struct {
			URL   string `json:"url"` // 大视频网页预览不提供文件，为 null（实测）
			Thumb string `json:"thumb"`
		} `json:"videos"`
	} `json:"media"`
}

func decodeTelegram(data []byte, t Target) (*post, error) {
	var m tgPost
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	channel, id, _ := strings.Cut(t.ResourceID, "/")
	if !strings.EqualFold(m.Channel, channel) || string(m.ID) != id {
		return nil, fmt.Errorf("%w：要 %s，拿到 %s/%s", errWrongItem, t.ResourceID, m.Channel, m.ID)
	}
	p := &post{
		ID:        t.ResourceID,
		URL:       "https://t.me/" + m.Channel + "/" + id,
		Text:      m.Text,
		CreatedAt: rfc3339UTC(m.Date),
		Author:    &postAuthor{Name: nonEmpty(m.Author, m.Channel), Handle: atHandle(m.Channel), URL: "https://t.me/" + m.Channel},
	}
	if m.Views > 0 {
		p.Stats = &postStats{Views: i64(int64(m.Views))}
	}
	for _, u := range m.Media.Photos {
		if u != "" {
			p.Images = append(p.Images, postImage{URL: u})
		}
	}
	// 作品只有一个 Video 位：没有照片时第一段视频进 Video（大视频没有文件，只留封面），其余收封面
	for _, v := range m.Media.Videos {
		switch {
		case p.Video == nil && len(p.Images) == 0:
			p.Video, p.Cover, p.Kind = &postVideo{URL: v.URL}, v.Thumb, "video"
		case v.Thumb != "":
			p.Images = append(p.Images, postImage{URL: v.Thumb})
		}
	}
	return p, nil
}
