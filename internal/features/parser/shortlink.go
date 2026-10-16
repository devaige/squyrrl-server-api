package parser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// ShortLinker 是可选接口：平台的分享短链（v.douyin.com、xhslink.com、b23.tv……）
// 把作品 ID 藏在一次跳转后面。服务端跟跳转拿到作品链接，再交给 Match 取 ID。
//
// 为什么不直接把短链交给「按分享链接取」的上游接口：缓存键得是作品 ID。同一作品的
// 每次分享都是一条新短链，拿短链当键，热门作品会被每个分享者各付一次上游的钱。
type ShortLinker interface {
	ShortLinkPatterns() []*regexp.Regexp
}

// ShortLinkResolver 把短链换成作品链接。done 判断某一跳是否已经是作品链接。
type ShortLinkResolver interface {
	Resolve(ctx context.Context, uri string, done func(string) bool) (string, error)
}

var errShortLink = errors.New("短链解析失败")

// httpResolver 只读跳转头，不读页面：拿到第一个能被平台识别的地址就停。
//
// 「拿到就停」是必需的，不是优化：小红书短链的第一跳就是带 xsec_token 的笔记链接，
// 再往下跟会撞上它的反爬页（实测 2026-10-16）。也因为只看 Location 头，服务端从不
// 下载目标站的页面 —— ADR-048 删掉服务端归档，正是因为代抓页面会被判成爬虫。
type httpResolver struct {
	client *http.Client
}

func newHTTPResolver() *httpResolver {
	return &httpResolver{client: &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 每一跳自己看，不让 client 自动跟
		},
	}}
}

// 几家短链服务按 UA 分流（桌面 UA 可能落到下载页），用移动浏览器的 UA 拿到的是作品页跳转。
const resolverUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

const maxShortLinkHops = 5

func (r *httpResolver) Resolve(ctx context.Context, uri string, done func(string) bool) (string, error) {
	cur := uri
	for range maxShortLinkHops {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", resolverUA)
		resp, err := r.client.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close() // 不读页面
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			return "", fmt.Errorf("%w：%s 返回 %d，没有跳转", errShortLink, cur, resp.StatusCode)
		}
		next, err := req.URL.Parse(loc) // Location 可以是相对地址
		if err != nil {
			return "", fmt.Errorf("%w：跳转地址无效: %v", errShortLink, err)
		}
		cur = next.String()
		if done(cur) {
			return cur, nil
		}
	}
	return "", fmt.Errorf("%w：跳转 %d 次仍未到作品页", errShortLink, maxShortLinkHops)
}

// FindShortLink 返回认领这条短链的平台。
func (r *Registry) FindShortLink(uri string) (Parser, bool) {
	for _, p := range r.parsers {
		sl, ok := p.(ShortLinker)
		if !ok {
			continue
		}
		for _, re := range sl.ShortLinkPatterns() {
			if re.MatchString(uri) {
				return p, true
			}
		}
	}
	return nil, false
}

// queryParam 取链接里的某个查询参数，解析失败时为空。
func queryParam(raw, key string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}
