package parser

import (
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// platform 是只靠付费上游取内容的平台（ADR-080）：它只负责认链接、取作品 ID。
// 内容来源全部是 api_endpoints 里的接口 + tikhub.go 里的适配器；一个接口都没启用时，
// 平台不进解析清单，客户端把它的链接当普通链接暂存。
//
// 正则里的 (?:^|[/.]) 要求域名前是边界，避免 nottiktok.com 这种被误认。
// 这些正则会原样进 manifest 给客户端（Dart RegExp），只用两边都支持的语法。
type platform struct {
	id        string
	canonical []*regexp.Regexp       // 第 1 个捕获组是作品 ID
	prefix    []string               // 可选，与 canonical 一一对应：一个平台有几种 ID 空间时（知乎回答 / 文章 / 想法）给 ID 加类型前缀
	short     []*regexp.Regexp       // 分享短链，需要先跟跳转
	normalize func(id string) string // 可选：把链接里的 ID 规整成接口要的形式
	extract   func(uri string) (string, bool)

	// priceFactor 可选：按链接给售价乘一个倍数，priceNote 是后台定价页上的说明。
	priceFactor func(t Target) int64
	priceNote   string
}

// PriceFactorer 是可选接口：某些链接注定要多调上游，售价按倍数收。
// 倍数由链接本身决定（不看缓存是否命中、实际调了几次），同一条链接永远同价。
type PriceFactorer interface {
	PriceFactor(t Target) int64
	PriceNote() string
}

func (p *platform) PriceFactor(t Target) int64 {
	if p.priceFactor == nil {
		return 1
	}
	return max(p.priceFactor(t), 1)
}

func (p *platform) PriceNote() string { return p.priceNote }

func (p *platform) Provider() string    { return p.id }
func (p *platform) SnippetType() string { return "special" }

func (p *platform) Match(uri string) (string, bool) {
	if p.extract != nil {
		return p.extract(uri)
	}
	for i, re := range p.canonical {
		if m := re.FindStringSubmatch(uri); m != nil {
			id := m[1]
			if p.normalize != nil {
				id = p.normalize(id)
			}
			if id != "" {
				if i < len(p.prefix) {
					id = p.prefix[i] + id
				}
				return id, true
			}
		}
	}
	return "", false
}

// MatchPatterns 导出作品链接与短链两类正则：客户端两种都该给「解析」按钮。
func (p *platform) MatchPatterns() []string {
	out := make([]string, 0, len(p.canonical)+len(p.short))
	for _, re := range append(append([]*regexp.Regexp{}, p.canonical...), p.short...) {
		out = append(out, re.String())
	}
	return out
}

func (p *platform) ShortLinkPatterns() []*regexp.Regexp { return p.short }

var (
	_ ManifestProvider = (*platform)(nil)
	_ ShortLinker      = (*platform)(nil)
)

func res(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// Platform IDs。同时是 parse_cache.provider、manifest id、parse_prices 的键、碎片的 subtype。
const (
	PlatformYouTube     = "youtube"
	PlatformTwitter     = "twitter"
	PlatformTikTok      = "tiktok"
	PlatformDouyin      = "douyin"
	PlatformXiaohongshu = "xiaohongshu"
	PlatformBilibili    = "bilibili"
	PlatformWeibo       = "weibo"
	PlatformInstagram   = "instagram"
	PlatformKuaishou    = "kuaishou"
	PlatformWechatMP    = "wechat_mp"

	// 第二批（2026-10-16）。Reddit 另有内置免费来源（reddit.go），付费接口排在它后面兜底。
	PlatformReddit         = "reddit"
	PlatformZhihu          = "zhihu"
	PlatformThreads        = "threads"
	PlatformWechatChannels = "wechat_channels"
	PlatformToutiao        = "toutiao" // 含西瓜视频：西瓜的作品就是头条的 group
	PlatformLemon8         = "lemon8"
	PlatformPipixia        = "pipixia"
	PlatformLinkedIn       = "linkedin"
	PlatformTelegram       = "telegram" // 公开频道消息；与 Bot 摄取的 telegram_message 碎片无关
)

// NewUpstreamPlatforms 返回只靠付费上游的平台，按注册顺序。
func NewUpstreamPlatforms() []Parser {
	return []Parser{
		&platform{id: PlatformTwitter, canonical: res(
			`(?:^|[/.])(?:twitter|x)\.com/[A-Za-z0-9_]+/status(?:es)?/(\d+)`,
		)},
		&platform{id: PlatformTikTok, canonical: res(
			`(?:^|[/.])tiktok\.com/@[^/?#]+/(?:video|photo)/(\d+)`,
			`(?:^|[/.])tiktok\.com/(?:embed(?:/v2)?|v)/(\d+)`,
		), short: res(
			`(?:^|//)(?:vm|vt)\.tiktok\.com/[A-Za-z0-9]+`,
			`(?:^|[/.])tiktok\.com/t/[A-Za-z0-9]+`,
		)},
		&platform{id: PlatformDouyin, canonical: res(
			`(?:^|[/.])(?:douyin|iesdouyin)\.com/(?:video|note|share/(?:video|note|slides))/(\d+)`,
			`(?:^|[/.])douyin\.com/[^#]*[?&]modal_id=(\d+)`,
		), short: res(
			`(?:^|//)v\.douyin\.com/[A-Za-z0-9_-]+`,
		)},
		&platform{id: PlatformXiaohongshu, canonical: res(
			`(?:^|[/.])xiaohongshu\.com/(?:explore|discovery/item|user/profile/[0-9a-f]{24})/([0-9a-f]{24})`,
		), short: res(
			`(?:^|//)xhslink\.com/[A-Za-z0-9/]+`,
		),
			// App 分享的链接带 type（normal / video），一次调用就够；网页端复制的没有 type，
			// 要先试视频接口、不是再走图文接口，最多两次调用 —— 按两倍收，保证这种链接也有毛利
			// （用户决定 2026-10-16）。
			priceFactor: func(t Target) int64 {
				if queryParam(t.Resolved, "type") == "" {
					return 2
				}
				return 1
			},
			priceNote: "网页端复制的链接（不带笔记类型）按 2 倍计费",
		},
		&platform{id: PlatformBilibili, canonical: res(
			`(?:^|[/.])bilibili\.com/video/(BV[0-9A-Za-z]{10}|av\d+)`,
		), short: res(
			`(?:^|//)b23\.tv/[A-Za-z0-9]+`,
		)},
		&platform{id: PlatformWeibo, canonical: res(
			`(?:^|[/.])weibo\.(?:com|cn)/(?:detail|status)/([0-9A-Za-z]+)`,
			`(?:^|[/.])weibo\.com/(?:u/)?\d+/([0-9A-Za-z]{9,16})`,
		), short: res(
			`(?:^|//)t\.cn/[A-Za-z0-9]+`,
			`(?:^|//)mapp\.api\.weibo\.cn/fx/[0-9a-f]+\.html`,
		), normalize: weiboMid},
		&platform{id: PlatformInstagram, canonical: res(
			`(?:^|[/.])instagram\.com/(?:[A-Za-z0-9_.]+/)?(?:p|reels?|tv)/([A-Za-z0-9_-]{5,})`,
		)},
		&platform{id: PlatformKuaishou, canonical: res(
			`(?:^|[/.])kuaishou\.com/short-video/([0-9a-z]+)`,
			`(?:^|[/.])(?:chenzhongtech|gifshow|kuaishou|kuaishouapp)\.com/fw/photo/([0-9a-z]+)`,
		), short: res(
			`(?:^|//)v\.kuaishou\.com/[A-Za-z0-9]+`,
			`(?:^|[/.])kuaishou\.com/f/[A-Za-z0-9_-]+`,
		)},
		&platform{id: PlatformWechatMP, canonical: res(
			`(?:^|//)mp\.weixin\.qq\.com/s(?:/[A-Za-z0-9_-]+|\?[^#]*__biz=)`,
		), extract: wechatArticleID},

		// ---- 第二批 ----
		// 知乎四种内容各一个接口、ID 各自一套，ID 带类型前缀；回答链接里也有问题 ID，回答排在问题前面
		&platform{id: PlatformZhihu, canonical: res(
			`(?:^|[/.])zhihu\.com/(?:question/\d+/)?answer/(\d+)`,
			`(?:^|//)zhuanlan\.zhihu\.com/p/(\d+)`,
			`(?:^|[/.])zhihu\.com/pin/(\d+)`,
			`(?:^|[/.])zhihu\.com/question/(\d+)`,
		), prefix: []string{"answer/", "p/", "pin/", "question/"}},
		&platform{id: PlatformThreads, canonical: res(
			`(?:^|[/.])threads\.(?:net|com)/(?:@[^/?#]+/post|t)/([A-Za-z0-9_-]{5,})`,
		)},
		// 视频号分享短链 weixin.qq.com/sph/<id> 跳到 finder-preview 页，两种都认，ID 是同一个
		&platform{id: PlatformWechatChannels, canonical: res(
			`(?:^|//)weixin\.qq\.com/sph/([A-Za-z0-9]+)`,
			`(?:^|//)channels\.weixin\.qq\.com/finder-preview/pages/sph\?(?:[^#]*&)?id=([A-Za-z0-9]+)`,
		)},
		// 头条文章 / 视频 / 微头条 + 西瓜视频；接口按链接类型分流（见 toutiaoKind）
		&platform{id: PlatformToutiao, canonical: res(
			`(?:^|[/.])toutiao\.com/(?:article|video|group|w|item)/(\d{10,})`,
			`(?:^|[/.])toutiao\.com/[ai](\d{15,})`,
			`(?:^|[/.])ixigua\.com/(?:video/)?(\d{15,})`,
		), short: res(
			`(?:^|//)m\.toutiao\.com/is/[A-Za-z0-9_-]+`,
			`(?:^|//)v\.ixigua\.com/[A-Za-z0-9_-]+`,
		)},
		&platform{id: PlatformLemon8, canonical: res(
			`(?:^|[/.])lemon8-app\.com/(?:[a-z]{2}/)?@[^/?#]+/(\d{15,})`,
		)},
		&platform{id: PlatformPipixia, canonical: res(
			`(?:^|//)h5\.pipix\.com/(?:ppx/)?item/(\d+)`,
		), short: res(
			`(?:^|//)h5\.pipix\.com/s/[A-Za-z0-9_-]+`,
		)},
		// 帖子链接里的数字是 activity / share / ugcPost 三种 URN 之一，各是一套 ID；文章（pulse）只有 slug
		&platform{id: PlatformLinkedIn, canonical: res(
			`(?:^|[/.])linkedin\.com/posts/[^/?#]*-activity-(\d+)`,
			`(?:^|[/.])linkedin\.com/posts/[^/?#]*-share-(\d+)`,
			`(?:^|[/.])linkedin\.com/posts/[^/?#]*-ugcPost-(\d+)`,
			`(?:^|[/.])linkedin\.com/feed/update/urn:li:activity:(\d+)`,
			`(?:^|[/.])linkedin\.com/pulse/([A-Za-z0-9_%-]+)`,
		), prefix: []string{"activity/", "share/", "ugcPost/", "activity/", "pulse/"}},
		&platform{id: PlatformTelegram, canonical: res(telegramPostPattern), extract: telegramPostID},
	}
}

// ---- 各平台 ID 的规整 ----

const base62Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// weiboMid 把微博链接里的 ID 规整成数字 mid。
//
// 网页链接 weibo.com/<uid>/<mblogid> 用的是 base62 短 ID（如 OCT5GxFOU），接口与
// 缓存键都用数字 mid。换算规则：从尾部每 4 位一组 base62 解码，除最前一组外补零到 7 位，
// 再按原顺序拼接。例：z0JH2lOMb → 3501756485200075。
func weiboMid(id string) string {
	if _, err := strconv.ParseUint(id, 10, 64); err == nil {
		return id
	}
	var parts []string
	for end := len(id); end > 0; end -= 4 {
		start := max(end-4, 0)
		var n int64
		for _, c := range id[start:end] {
			i := strings.IndexRune(base62Alphabet, c)
			if i < 0 {
				return ""
			}
			n = n*62 + int64(i)
		}
		s := strconv.FormatInt(n, 10)
		if start > 0 {
			s = strings.Repeat("0", max(7-len(s), 0)) + s
		}
		parts = append([]string{s}, parts...)
	}
	return strings.TrimLeft(strings.Join(parts, ""), "0")
}

const instagramAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// instagramMediaID 把帖子 shortcode 换算成数字媒体 ID（shortcode 就是媒体 ID 的 64 进制）。
// 私密帖子的 shortcode 超过 11 位、后面拼了别的东西，换不出来时返回空。
func instagramMediaID(code string) string {
	if len(code) > 11 {
		return ""
	}
	n := new(big.Int)
	for _, c := range code {
		i := strings.IndexRune(instagramAlphabet, c)
		if i < 0 {
			return ""
		}
		n.Mul(n, big.NewInt(64)).Add(n, big.NewInt(int64(i)))
	}
	return n.String()
}

// telegramPostPattern 是公开频道的单条消息：t.me/<频道>/<消息号>，网页预览 t.me/s/<频道>/<消息号> 同理。
// 频道用户名 5–32 位、字母开头，这也排除了 t.me/c/…（私有频道，只有成员能看）和 joinchat 这类路径。
const telegramPostPattern = `(?:^|//)(?:www\.)?(?:t|telegram)\.me/(?:s/)?([A-Za-z][A-Za-z0-9_]{4,31})/(\d+)`

var telegramPostRe = regexp.MustCompile(telegramPostPattern)

// telegramPostID 返回「频道/消息号」。频道用户名不分大小写，统一小写，同一条消息只占一个缓存键。
func telegramPostID(uri string) (string, bool) {
	m := telegramPostRe.FindStringSubmatch(uri)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]) + "/" + m[2], true
}

// wechatArticleID 取公众号文章的稳定 ID：短链形式 /s/<token> 用 token，
// 长链形式用 __biz + mid + idx（同一篇文章的两种链接会落成两个缓存键，可以接受）。
func wechatArticleID(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Host != "mp.weixin.qq.com" {
		return "", false
	}
	if token, ok := strings.CutPrefix(u.Path, "/s/"); ok && token != "" && !strings.Contains(token, "/") {
		return "s:" + token, true
	}
	if u.Path == "/s" {
		q := u.Query()
		if biz, mid := q.Get("__biz"), q.Get("mid"); biz != "" && mid != "" {
			return biz + ":" + mid + ":" + q.Get("idx"), true
		}
	}
	return "", false
}
