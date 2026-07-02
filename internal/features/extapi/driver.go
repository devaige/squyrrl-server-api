package extapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxRespBytes 限制读取的响应体大小，防止个别 endpoint 返回超大 body 拖垮内存 / 撑爆存档。
const maxRespBytes = 1 << 20 // 1MB

var envPlaceholder = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// subst 展开占位符。resolveEnv=false 时保留 ${env:VAR} 原样 —— 存档快照用此版本，
// 保证 API key 等密钥永远不会写进 api_samples。
func subst(s, uri, resourceID string, resolveEnv bool) string {
	s = strings.ReplaceAll(s, "${resource_id}", resourceID)
	s = strings.ReplaceAll(s, "${uri}", uri)
	if resolveEnv {
		s = envPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
			return os.Getenv(envPlaceholder.FindStringSubmatch(m)[1])
		})
	}
	return s
}

// callTrace 是一次调用的请求/响应快照，用于失败排障与线上采样存档。
type callTrace struct {
	request   map[string]any
	response  map[string]any
	status    int
	latencyMs int
}

// callEndpoint 按 config 构造并发起一次上游调用，返回中立结果 + 快照。
// 不做扣费 / 存档 / 兜底，这些由 Service.Fetch 编排。
func (s *Service) callEndpoint(ctx context.Context, ep Endpoint, cfg EndpointConfig, uri, resourceID string) (*FetchResult, *callTrace, error) {
	method := cfg.Method
	if method == "" {
		method = http.MethodGet
	}
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	u, err := url.Parse(subst(cfg.URLTemplate, uri, resourceID, true))
	if err != nil {
		return nil, nil, fmt.Errorf("endpoint %s url 无效: %w", ep.Slug, err)
	}
	q := u.Query()
	archQuery := map[string]string{}
	for k, v := range cfg.Query {
		q.Set(k, subst(v, uri, resourceID, true))
		archQuery[k] = subst(v, uri, resourceID, false)
	}
	u.RawQuery = q.Encode()

	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(subst(cfg.Body, uri, resourceID, true))
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, u.String(), body)
	if err != nil {
		return nil, nil, err
	}
	archHeaders := map[string]string{}
	for k, v := range cfg.Headers {
		req.Header.Set(k, subst(v, uri, resourceID, true))
		archHeaders[k] = subst(v, uri, resourceID, false) // 存档保留模板，密钥不落库
	}

	// 存档请求快照：URL 用未解析 env 的模板版本，彻底杜绝密钥泄漏进 DB
	archReq := map[string]any{
		"method":  method,
		"url":     subst(cfg.URLTemplate, uri, resourceID, false),
		"headers": archHeaders,
		"query":   archQuery,
	}
	if cfg.Body != "" {
		archReq["body"] = subst(cfg.Body, uri, resourceID, false)
	}

	start := time.Now()
	resp, err := s.client.Do(req)
	trace := &callTrace{request: archReq, latencyMs: int(time.Since(start).Milliseconds())}
	if err != nil {
		return nil, trace, fmt.Errorf("endpoint %s 请求失败: %w", ep.Slug, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	trace.status = resp.StatusCode
	trace.response = map[string]any{"status": resp.StatusCode, "body": jsonOrString(raw)}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, trace, fmt.Errorf("endpoint %s 返回状态 %d", ep.Slug, resp.StatusCode)
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, trace, fmt.Errorf("endpoint %s 响应非 JSON: %w", ep.Slug, err)
	}
	rm := cfg.ResponseMap
	return &FetchResult{
		Title:        getPath(parsed, rm.Title),
		Description:  getPath(parsed, rm.Description),
		ThumbnailURL: getPath(parsed, rm.ThumbnailURL),
		AuthorName:   getPath(parsed, rm.AuthorName),
		AuthorURL:    getPath(parsed, rm.AuthorURL),
		Raw:          json.RawMessage(raw),
	}, trace, nil
}

// getPath 用点路径从已解析的 JSON 里取值，支持对象键与数组下标，如 "items.0.snippet.title"。
// 任一段缺失即返回空串，让调用方拿到「零值」而非 panic。
func getPath(v any, path string) string {
	if path == "" {
		return ""
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return ""
			}
			cur = node[idx]
		default:
			return ""
		}
		if cur == nil {
			return ""
		}
	}
	return toStr(cur)
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// jsonOrString 尽量把响应体存成结构化 JSON（便于 JSONB 检索），非 JSON 则退化为字符串。
func jsonOrString(b []byte) any {
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	return string(b)
}
