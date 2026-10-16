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
	short     []*regexp.Regexp       // 分享短链，需要先跟跳转
	normalize func(id string) string // 可选：把链接里的 ID 规整成接口要的形式
	extract   func(uri string) (string, bool)
}

func (p *platform) Provider() string    { return p.id }
func (p *platform) SnippetType() string { return "special" }

func (p *platform) Match(uri string) (string, bool) {
	if p.extract != nil {
		return p.extract(uri)
	}
	for _, re := range p.canonical {
		if m := re.FindStringSubmatch(uri); m != nil {
			id := m[1]
			if p.normalize != nil {
				id = p.normalize(id)
			}
			if id != "" {
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
		)},
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
