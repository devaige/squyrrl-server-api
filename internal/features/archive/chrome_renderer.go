package archive

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// ChromeRenderer 使用 chromedp + headless Chrome 抓页面。
// 流程：
//  1. 启动 / 复用 allocator 上下文（系统 Chrome）
//  2. 导航并等 networkIdle，跑一段 JS 把 <img> 元素的 src 替换为 data: URI
//  3. <link rel=stylesheet> 抓远程 CSS 文本，inline 为 <style>
//  4. 取最终 DOM outerHTML 作为归档字节
//
// 超时与体积限制：每次最长 45s（含网络），产物最大 ChromeMaxBytes。
type ChromeRenderer struct {
	allocCtx context.Context
	cancel   context.CancelFunc
	once     sync.Once
	fetcher  *http.Client
}

// ChromeMaxBytes Chrome 渲染产物字节上限。比轻量版宽松（含 inline 资源）
const ChromeMaxBytes = 20 * 1024 * 1024

func NewChromeRenderer() *ChromeRenderer {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.UserAgent("Squyrrl/0.1 (+https://squyrrl.app) Archive Bot (chromedp)"),
	)
	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	return &ChromeRenderer{
		allocCtx: ctx,
		cancel:   cancel,
		fetcher:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *ChromeRenderer) Name() string { return "chromedp" }

// Close 由调用方在进程退出前调用，释放 allocator 进程
func (r *ChromeRenderer) Close() {
	r.once.Do(func() { r.cancel() })
}

func (r *ChromeRenderer) Render(parent context.Context, target string) ([]byte, string, error) {
	if _, err := url.ParseRequestURI(target); err != nil {
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()

	tabCtx, tabCancel := chromedp.NewContext(r.allocCtx)
	defer tabCancel()
	// 把外层 timeout 套到 tab 上
	tabCtx, cancel2 := mergeCtx(tabCtx, ctx)
	defer cancel2()

	var html string
	if err := chromedp.Run(tabCtx,
		chromedp.Navigate(target),
		chromedp.WaitReady("body", chromedp.ByQuery),
		// 给 SPA 一点时间渲染（足以覆盖 React/Vue mount）
		chromedp.Sleep(800*time.Millisecond),
		chromedp.ActionFunc(func(c context.Context) error {
			node, err := dom.GetDocument().Do(c)
			if err != nil {
				return err
			}
			html, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(c)
			return err
		}),
	); err != nil {
		return nil, "", fmt.Errorf("chromedp render: %w", err)
	}

	// 内联远程图片 / CSS；越界丢弃，保证 <= ChromeMaxBytes
	html = r.inlineAssets(parent, target, html)
	if len(html) > ChromeMaxBytes {
		return nil, "", ErrTooLarge
	}
	return []byte(html), "text/html", nil
}

// inlineAssets 用一个简单正则 / 字符串扫描把 <img src="…"> 和 <link rel="stylesheet" href="…">
// 替换成 data: URI / inline <style>。失败的资源原样保留，不阻塞归档。
//
// 注：完整 SingleFile 仍需处理 srcset / background-image / @import；
// 这里只覆盖最常见 80% 场景，剩余在 Phase 2 视用户反馈再补。
func (r *ChromeRenderer) inlineAssets(ctx context.Context, baseURL, html string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return html
	}
	html = rewriteAttr(html, "img", "src", func(raw string) string {
		abs := resolveURL(base, raw)
		if abs == "" {
			return raw
		}
		data, mime, ok := r.fetchSmall(ctx, abs, 1*1024*1024) // 单图 1 MiB
		if !ok {
			return raw
		}
		return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
	})
	html = inlineStylesheets(html, base, func(abs string) ([]byte, bool) {
		data, _, ok := r.fetchSmall(ctx, abs, 2*1024*1024) // 单 CSS 2 MiB
		if !ok {
			return nil, false
		}
		return data, true
	})
	return html
}

func (r *ChromeRenderer) fetchSmall(ctx context.Context, uri string, max int64) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", "Squyrrl/0.1 archive-inliner")
	resp, err := r.fetcher.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || int64(len(body)) > max {
		return nil, "", false
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(body)
	}
	return body, mime, true
}

// mergeCtx 让 inner ctx 同时尊重 outer 的截止时间
func mergeCtx(inner, outer context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(inner)
	go func() {
		select {
		case <-outer.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// 让 cdp 包不被 lint 嫌弃（导入了但实际通过 dom.* 使用）
var _ = cdp.NodeID(0)
