package archive

import (
	"net/url"
	"regexp"
	"strings"
)

// HTML 改写工具：纯字符串扫描，不引入完整 HTML 解析器（golang.org/x/net/html 体积大）。
// 覆盖 80% 主流站点的 <img src=> 与 <link rel="stylesheet" href=> 用法，
// 边缘场景（srcset、@import、内嵌 SVG）原样保留，由 chromedp 浏览器执行兜底。

var (
	reTag = regexp.MustCompile(`(?is)<(img|link)\b[^>]*>`)
	reAttr = regexp.MustCompile(`(?i)\b([a-z][a-z0-9-]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s/>]+))`)
)

// rewriteAttr 找出所有 <tag …> 标签，调用 transform 把 attr 的值替换为新值。
func rewriteAttr(html, tag, attr string, transform func(string) string) string {
	tagLower := strings.ToLower(tag)
	attrLower := strings.ToLower(attr)
	return reTag.ReplaceAllStringFunc(html, func(m string) string {
		// 仅处理目标标签
		open := strings.IndexAny(m, " \t\n\r/>")
		if open < 0 {
			return m
		}
		if !strings.EqualFold(m[1:open], tagLower) {
			return m
		}
		return reAttr.ReplaceAllStringFunc(m, func(am string) string {
			parts := reAttr.FindStringSubmatch(am)
			if len(parts) == 0 || !strings.EqualFold(parts[1], attrLower) {
				return am
			}
			val := parts[3]
			if val == "" {
				val = parts[4]
			}
			if val == "" {
				val = parts[5]
			}
			newVal := transform(val)
			if newVal == val {
				return am
			}
			// 用双引号包，转义双引号
			escaped := strings.ReplaceAll(newVal, `"`, `&quot;`)
			return parts[1] + `="` + escaped + `"`
		})
	})
}

// inlineStylesheets 把 <link rel="stylesheet" href="…"> 替换成 <style>…</style>
func inlineStylesheets(html string, base *url.URL, fetch func(absURL string) ([]byte, bool)) string {
	return reTag.ReplaceAllStringFunc(html, func(m string) string {
		open := strings.IndexAny(m, " \t\n\r/>")
		if open < 0 || !strings.EqualFold(m[1:open], "link") {
			return m
		}
		isStylesheet := false
		href := ""
		for _, p := range reAttr.FindAllStringSubmatch(m, -1) {
			val := p[3]
			if val == "" {
				val = p[4]
			}
			if val == "" {
				val = p[5]
			}
			switch strings.ToLower(p[1]) {
			case "rel":
				if strings.Contains(strings.ToLower(val), "stylesheet") {
					isStylesheet = true
				}
			case "href":
				href = val
			}
		}
		if !isStylesheet || href == "" {
			return m
		}
		abs := resolveURL(base, href)
		if abs == "" {
			return m
		}
		data, ok := fetch(abs)
		if !ok {
			return m
		}
		return "<style>" + string(data) + "</style>"
	})
}

// resolveURL 把相对 URL 解析为绝对；data:/about: 直接保留
func resolveURL(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	low := strings.ToLower(raw)
	if strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "about:") || strings.HasPrefix(low, "javascript:") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}
