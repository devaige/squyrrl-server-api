package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// GenericOGProvider 是 URL 兜底解析器：
//   - Match 接受任何 http(s):// 链接，但应注册在 registry 末尾
//   - Parse 抓取 HTML，解析 <title> 与 og:* / name= meta 标签
//   - 输出的 SnippetType="url"，对应链接（URL）碎片
//
// 只提取元信息、不下载图片、不内联资源；目的是给客户端一个有意义的卡片预览。
// （服务端完整存档已移除，见 memory.md ADR-048；完整 SingleFile 归档拟由客户端承担。）
type GenericOGProvider struct {
	client *http.Client
}

func NewGenericOGProvider() *GenericOGProvider {
	return &GenericOGProvider{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *GenericOGProvider) Provider() string    { return "url" }
func (p *GenericOGProvider) SnippetType() string { return "url" }

func (p *GenericOGProvider) Match(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	// resource_id = full URL；用于 parse_cache 命中（同一 URL 不同用户复用）
	return uri, true
}

func (p *GenericOGProvider) Parse(ctx context.Context, uri, _ string) (*ParseResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.com)")
	req.Header.Set("Accept", "text/html,*/*;q=0.8")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	// 防御性限制：只读前 1MiB 用于元信息提取
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	meta := extractMeta(body)
	titleTag := extractTitle(body)

	finalTitle := firstNonEmpty(meta["og:title"], meta["twitter:title"], titleTag)
	finalDesc := firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"])

	payload, _ := json.Marshal(map[string]any{
		"url":            uri,
		"og_title":       meta["og:title"],
		"og_description": meta["og:description"],
		"og_image":       meta["og:image"],
		"og_site_name":   meta["og:site_name"],
		"og_type":        meta["og:type"],
		"twitter_card":   meta["twitter:card"],
		"final_url":      resp.Request.URL.String(),
		"http_status":    resp.StatusCode,
	})
	source, _ := json.Marshal(map[string]any{
		"provider": "url",
		"site":     firstNonEmpty(meta["og:site_name"], hostOf(uri)),
	})

	return &ParseResult{
		Title:       toPtr(finalTitle),
		Description: toPtr(finalDesc),
		Payload:     payload,
		SourceData:  source,
		// og:image 是相对/协议相对时不放行（mediaAsset 只认绝对 http(s)），避免客户端下载失败。
		Assets: mediaAsset(meta["og:image"], "image", ""),
	}, nil
}

// =============================================================================
// 解析工具
// =============================================================================

var (
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaRe  = regexp.MustCompile(`(?is)<meta\b[^>]*?>`)
	attrRe  = regexp.MustCompile(`(?is)([a-zA-Z][\w:-]*)\s*=\s*"([^"]*)"|([a-zA-Z][\w:-]*)\s*=\s*'([^']*)'`)
)

// extractTitle 抓 <title> 标签内文本（不解 nested 但够用）
func extractTitle(body []byte) string {
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

// extractMeta 扫描所有 <meta>，按 property/name → content 收集
func extractMeta(body []byte) map[string]string {
	out := make(map[string]string)
	for _, raw := range metaRe.FindAll(body, -1) {
		attrs := parseAttrs(string(raw))
		key := attrs["property"]
		if key == "" {
			key = attrs["name"]
		}
		if key == "" {
			continue
		}
		key = strings.ToLower(key)
		if _, ok := out[key]; ok {
			continue // 首个出现优先
		}
		out[key] = strings.TrimSpace(html.UnescapeString(attrs["content"]))
	}
	return out
}

func parseAttrs(tag string) map[string]string {
	attrs := make(map[string]string)
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		if m[1] != "" {
			attrs[strings.ToLower(m[1])] = m[2]
		} else {
			attrs[strings.ToLower(m[3])] = m[4]
		}
	}
	return attrs
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func toPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func hostOf(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Host
}

// 单测用兜底（避免 unused warning）
var _ = errors.New
