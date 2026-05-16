package archive

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// Renderer 把一个 URL 渲染成可离线阅读的字节流（一般是 HTML）。
// 两个实现：
//   - LightRenderer：单次 GET，原 HTML，不执行 JS（默认）
//   - ChromeRenderer：chromedp 跑 headless Chrome，等 DOMContentLoaded，
//     inline 同源图片为 data: URI、外联 CSS 文本化，产出单文件 HTML
type Renderer interface {
	Render(ctx context.Context, url string) (body []byte, mime string, err error)
	Name() string
}

// LightRenderer：保留原始轻量逻辑（5 MiB 上限，单次 GET）
type LightRenderer struct {
	client *http.Client
}

func NewLightRenderer() *LightRenderer {
	return &LightRenderer{client: &http.Client{Timeout: 20 * time.Second}}
}

func (r *LightRenderer) Name() string { return "light" }

func (r *LightRenderer) Render(ctx context.Context, uri string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.app) Archive Bot")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", &httpError{Code: resp.StatusCode, URL: uri}
	}

	limited := io.LimitReader(resp.Body, MaxArchiveBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > MaxArchiveBytes {
		return nil, "", ErrTooLarge
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(body)
	}
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return body, mime, nil
}
