package parser

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// post 是社交平台「作品」的统一形状（ADR-080）。
//
// 所有只靠付费上游的平台都转成它：同一平台不管由哪个接口产出、哪个上游提供，
// payload 的字段集合一致；不同平台之间也一致，客户端将来做专属卡片时只认这一种。
// 这里**不能**出现上游的痕迹（上游名、接口、上游的请求 ID）。
type post struct {
	URL       string      `json:"url"` // 规整后的作品链接（去掉分享追踪参数），详情页的跳转入口
	ID        string      `json:"id"`
	Kind      string      `json:"kind"` // video | image | text | article
	Title     string      `json:"title,omitempty"`
	Text      string      `json:"text,omitempty"`
	CreatedAt string      `json:"created_at,omitempty"` // RFC 3339
	Author    *postAuthor `json:"author,omitempty"`
	Cover     string      `json:"cover,omitempty"`
	Images    []postImage `json:"images,omitempty"`
	Video     *postVideo  `json:"video,omitempty"`
	Stats     *postStats  `json:"stats,omitempty"`

	version string // 内容版本标记，进 ParseResult.Version，不进 payload
}

type postAuthor struct {
	Name   string `json:"name,omitempty"`
	Handle string `json:"handle,omitempty"`
	ID     string `json:"id,omitempty"`
	URL    string `json:"url,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

type postImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	// 实况图片（抖音的「实况」/ iOS 的 Live Photo）的那段动效。有它时 URL 一定是 JPEG：
	// 客户端把两者合成一张实况照片（Motion Photo）存成一个附件
	Live *postVideo `json:"live,omitempty"`
	// 图文里的一段视频（抖音图文混排）：这一张存的是视频，URL 是它的首帧，做视频的缩略图
	Video *postVideo `json:"video,omitempty"`
}

type postVideo struct {
	URL        string `json:"url,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
}

// postStats 用指针区分「0」与「上游没给」。
type postStats struct {
	Likes    *int64 `json:"likes,omitempty"`
	Comments *int64 `json:"comments,omitempty"`
	Shares   *int64 `json:"shares,omitempty"`
	Views    *int64 `json:"views,omitempty"`
	Collects *int64 `json:"collects,omitempty"`
}

func i64(n int64) *int64 { return &n }

// descriptionLimit 是碎片 description 的上限；全文在 payload.text 里。
// 公众号文章正文可达数万字，整篇塞进列表卡片的文本字段没有意义。
const descriptionLimit = 2000

// result 把作品组装成 ParseResult：标题、正文、payload、来源信息、待下载的文件。
func (p *post) result(provider string) *ParseResult {
	p.Text = strings.TrimSpace(p.Text)
	p.Title = strings.TrimSpace(p.Title)
	if p.Kind == "" {
		switch {
		case p.Video != nil && p.Video.URL != "":
			p.Kind = "video"
		case len(p.Images) > 0:
			p.Kind = "image"
		default:
			p.Kind = "text"
		}
	}

	if p.Stats != nil && *p.Stats == (postStats{}) {
		p.Stats = nil // 上游一个计数都没给（知乎回答），不写空对象
	}

	res := &ParseResult{Version: p.version}
	if p.Title != "" {
		t := p.Title
		res.Title = &t
	}
	if p.Text != "" {
		d := truncateRunes(p.Text, descriptionLimit)
		res.Description = &d
	}
	if len([]rune(p.Text)) <= descriptionLimit && p.Kind != "article" {
		// 正文已经完整放在 description 里，payload 不再存第二份
		p.Text = ""
	}

	res.Payload, _ = json.Marshal(p)
	src := map[string]any{"provider": provider}
	if a := p.Author; a != nil {
		putNonEmpty(src, "author_name", a.Name)
		putNonEmpty(src, "author_handle", a.Handle)
		putNonEmpty(src, "author_url", a.URL)
		putNonEmpty(src, "author_avatar_url", a.Avatar)
	}
	res.SourceData, _ = json.Marshal(src)

	// 下载顺序：封面 → 正文图片 → 视频。图文作品的封面就是第一张图，不重复下载。
	if p.Kind == "video" || p.Kind == "article" || len(p.Images) == 0 {
		res.Assets = append(res.Assets, mediaAsset(p.Cover, "thumbnail", "")...)
	}
	for _, img := range p.Images {
		if img.Video != nil {
			// 首帧紧挨在视频前面，同视频作品的封面一样做它的缩略图、不另存
			res.Assets = append(res.Assets, mediaAsset(img.URL, "thumbnail", "")...)
			res.Assets = append(res.Assets, mediaAsset(img.Video.URL, "media", "video/mp4")...)
			continue
		}
		res.Assets = append(res.Assets, mediaAsset(img.URL, "image", "")...)
		// 动效紧跟在它的静图后面，客户端按相邻配对
		if img.Live != nil {
			res.Assets = append(res.Assets, mediaAsset(img.Live.URL, "motion", "video/mp4")...)
		}
	}
	if p.Video != nil {
		res.Assets = append(res.Assets, mediaAsset(p.Video.URL, "media", "video/mp4")...)
	}
	res.CacheTTL = assetsExpireIn(res.Assets, time.Now())
	return res
}

func putNonEmpty(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- 媒体地址过期时间 ----

// 实测（2026-10-16）各平台媒体地址的过期标记：
//   - 抖音 / TikTok 视频：路径里 32 位哈希后的 8 位十六进制时间戳，约 1–6 小时；
//     图片：x-expires（十进制秒）；
//   - Instagram：oe（十六进制秒），约 4 天；
//   - 微博视频：Expires（十进制秒），约 1–3 小时。
var bytedanceExpiryPath = regexp.MustCompile(`/[0-9a-f]{32}/([0-9a-f]{8})/`)

// expiryMargin 给客户端留出下载时间：解析返回后客户端立刻下载，但大视频要几十秒。
const expiryMargin = 10 * time.Minute

// assetsExpireIn 返回这批文件里最早过期的那个还剩多久（已扣除余量）。
// 没有可识别的过期标记时返回 0，缓存照常按全局保留期。
func assetsExpireIn(assets []Asset, now time.Time) time.Duration {
	var earliest time.Time
	for _, a := range assets {
		if t, ok := urlExpiry(a.URL); ok && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		return 0
	}
	// 已经快过期的也给一个很短的正数，而不是 0（0 表示「不限」）
	return max(earliest.Sub(now)-expiryMargin, time.Minute)
}

func urlExpiry(raw string) (time.Time, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	q := u.Query()
	var times []int64
	for _, k := range []string{"x-expires", "expires", "Expires", "expire"} {
		if n, err := strconv.ParseInt(q.Get(k), 10, 64); err == nil {
			times = append(times, n)
		}
	}
	if n, err := strconv.ParseInt(q.Get("oe"), 16, 64); err == nil {
		times = append(times, n)
	}
	if m := bytedanceExpiryPath.FindStringSubmatch(u.Path); m != nil {
		if n, err := strconv.ParseInt(m[1], 16, 64); err == nil {
			times = append(times, n)
		}
	}
	var best int64
	for _, t := range times {
		// 只认落在合理区间里的时间戳，避免把别的数字参数当成过期时间
		if t > 1_500_000_000 && t < 4_000_000_000 && (best == 0 || t < best) {
			best = t
		}
	}
	if best == 0 {
		return time.Time{}, false
	}
	return time.Unix(best, 0), true
}

// ---- 小工具 ----

// flexInt 接受数字或数字字符串（YouTube 的播放数是字符串，别家是数字）。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(n)
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(n)
	}
	return nil // 认不出的格式当作没给，而不是让整条解析失败
}

// flexString 接受字符串或数字（同一个 ID 字段在不同接口里类型不同）。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

func unixRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	if sec > 1e12 { // 毫秒
		sec /= 1000
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// rfc3339UTC 把带时区的 RFC 3339 时间统一成 UTC 写法（YouTube 给的是 -07:00）。
func rfc3339UTC(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// rubyDateRFC3339 转换 "Tue Jul 02 15:59:23 +0000 2024" 这种（X、微博都用）。
func rubyDateRFC3339(s string) string {
	t, err := time.Parse(time.RubyDate, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// httpsURL 把 http:// 换成 https://（B 站、快手、公众号的 CDN 都支持 https，
// 客户端在 iOS / Android 上默认拒绝明文请求）。
func httpsURL(s string) string {
	if rest, ok := strings.CutPrefix(s, "http://"); ok {
		return "https://" + rest
	}
	return s
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// plainText 去掉 HTML 标签并反转义实体（微博 App 的正文带链接标签）。
func plainText(s string) string {
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	return html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}

func first[T any](xs []T) (T, bool) {
	var zero T
	if len(xs) == 0 {
		return zero, false
	}
	return xs[0], true
}

func firstURL(xs []string) string {
	u, _ := first(xs)
	return u
}

// displayURL 在几组候选地址里挑一张各端都能显示的图：按 imageRank 取档最好的，同档取先出现的
// （所以前一组优先）；没有候选时为空。
//
// 字节系（抖音 / TikTok App、Lemon8）的 url_list 常把客户端解不了的格式排在最前：TikTok 封面是
// heic、heic、jpeg；抖音图文作品前两个是 VVC（H.266）编码的原图（路径带 _vvic，扩展名 .image），
// 第三个才是 1920 宽的 jpeg。原先取第一个，测试反馈图文作品存下来后列表一张图都出不来、Android
// 报解码失败（2026-10-18，作品 7692800900575176625）。
//
// 只剩解不了的格式时照给：苹果端能显示 HEIC，总比没有图好。
func displayURL(lists ...[]string) string {
	best, rank := "", 3
	for _, xs := range lists {
		for _, u := range xs {
			if u == "" {
				continue
			}
			if r := imageRank(u); r < rank {
				best, rank = u, r
			}
		}
	}
	return best
}

// imageRank 按客户端能不能显示给图片地址分档：0 = jpeg / png / webp / gif，各端都能解；
// 1 = 认不出格式（没有扩展名、字节的 .image 之类）；2 = HEIC 或字节的 VVC 图，Android、网页与
// 桌面端都解不了（见客户端 thumbnailer）。
func imageRank(u string) int {
	path, _, _ := strings.Cut(u, "?")
	path = strings.ToLower(path)
	switch {
	case isHEIC(path), strings.Contains(path, "_vvic"):
		return 2
	}
	for _, ext := range []string{".jpeg", ".jpg", ".png", ".webp", ".gif"} {
		if strings.HasSuffix(path, ext) {
			return 0
		}
	}
	return 1
}
