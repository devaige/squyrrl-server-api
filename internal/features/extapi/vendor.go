package extapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Request 是适配器声明的一次上游调用。鉴权头与响应外壳由 vendor 处理 ——
// 适配器从头到尾碰不到密钥，存档里自然也不会有。
type Request struct {
	Method string // 默认 GET
	URL    string // 绝对地址
	Query  map[string]string
	Body   []byte // 非空时按 application/json 发送
}

// 失败类别：写进 parse_attempts.error_kind，后台按它筛选和统计失败原因。
// 是否计费看的不是类别而是 CallResult.Billed —— 同样叫「失败」，上游收不收钱是两回事。
const (
	KindNoAdapter     = "no_adapter"     // 跳过：代码里没有这个 slug 的适配器，或适配器与账户不是同一上游
	KindNoBalance     = "no_balance"     // 跳过：账户余额耗尽
	KindNotApplicable = "not_applicable" // 跳过：这条链接不归这个接口（如小红书视频接口只接视频笔记）
	KindConfig        = "config"         // 失败：接口配置或上游登记有误
	KindNoCredential  = "no_credential"  // 失败：没配该上游的密钥
	KindNetwork       = "network"
	KindTimeout       = "timeout"
	KindHTTPStatus    = "http_status"  // 非 2xx，具体码见 http_status；一般不计费
	KindBadResponse   = "bad_response" // 2xx 但不是 JSON、外壳不对或超长；已计费
	KindEmpty         = "empty"        // 2xx 但没有数据；已计费
	KindDecode        = "decode"       // 适配器转换失败或结果不可用；已计费
	KindBuiltin       = "builtin"      // 内置免费来源报错
)

// CallError 是一次调用失败的类别与说明。说明只进解析记录，不回给用户 ——
// 里面会有上游的名字和报错原文。
type CallError struct {
	Kind string
	Msg  string
}

func (e *CallError) Error() string { return e.Kind + ": " + e.Msg }

// CallResult 是一次上游调用的结果。
//
// Err 为 nil 只说明 HTTP 层面成功：内容能不能用，要等适配器 Decode 之后才知道，
// 但那时钱已经花了 —— 所以 Billed / CostMicros 与 Err 相互独立。
type CallResult struct {
	Data         []byte // 去掉上游外壳后的数据
	Billed       bool
	CostMicros   int64
	BalanceAfter int64 // 记账后的账户余额（基于调用前读到的余额推算）
	HTTPStatus   int
	LatencyMs    int
	Err          *CallError
	SampleID     *uuid.UUID // archive_live 打开时这次调用存下的线上采样，解析记录据此点回原始响应
}

// Credentials 是各上游的 API 密钥，来自环境变量（config.go），永不落库。
type Credentials struct {
	TikHub   string
	RapidAPI string
}

func (c Credentials) key(vendor string) string {
	switch vendor {
	case VendorTikHub:
		return c.TikHub
	case VendorRapidAPI:
		return c.RapidAPI
	}
	return ""
}

const (
	VendorTikHub   = "tikhub"
	VendorRapidAPI = "rapidapi"
)

// vendor 封装一个上游的鉴权方式与响应外壳。上游只有两种形态（直连、聚合），
// 区别全在这两个方法里；新增上游 = 新增一个实现 + 在 vendors 里登记。
type vendor interface {
	authorize(req *http.Request, key string)
	// unwrap 剥掉外壳返回数据本体，并判断这次调用上游有没有计费。
	unwrap(status int, body []byte) (data []byte, billed bool, err *CallError)
}

var vendors = map[string]vendor{
	VendorTikHub:   tikhub{},
	VendorRapidAPI: rapidAPI{},
}

// Vendors 返回已登记的上游，供后台建账户时选择。
func Vendors() []string {
	out := make([]string, 0, len(vendors))
	for v := range vendors {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// tikhub：直连上游，一个账户、所有接口共用余额。
//
// 响应外壳是 {code, message, data, router, params, ...}。HTTP 200 即计费 ——
// 它的成功响应自己就写着「请求成功，本次请求将被计费」。出错用 HTTP 状态码表达
// （400/401/402 余额不足/403/404/429/500），这些不计费。
type tikhub struct{}

func (tikhub) authorize(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
}

func (tikhub) unwrap(status int, body []byte) ([]byte, bool, *CallError) {
	if status < 200 || status >= 300 {
		return nil, false, &CallError{Kind: KindHTTPStatus, Msg: errorMessage(body)}
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, true, &CallError{Kind: KindBadResponse, Msg: "响应不是 JSON: " + err.Error()}
	}
	if env.Code != 0 && env.Code != http.StatusOK {
		return nil, true, &CallError{Kind: KindBadResponse, Msg: fmt.Sprintf("外壳 code=%d: %s", env.Code, errorMessage(body))}
	}
	if isEmptyJSON(env.Data) {
		return nil, true, &CallError{Kind: KindEmpty, Msg: "data 为空"}
	}
	return env.Data, true, nil
}

// rapidAPI：聚合平台，每个子提供商一份独立订阅、一个账户。
//
// 不加外壳，响应体就是子提供商的原样输出；鉴权是统一的 key + 子提供商的 host。
// 2xx 计入订阅配额，按计费处理。
type rapidAPI struct{}

func (rapidAPI) authorize(req *http.Request, key string) {
	req.Header.Set("x-rapidapi-key", key)
	req.Header.Set("x-rapidapi-host", req.URL.Host)
}

func (rapidAPI) unwrap(status int, body []byte) ([]byte, bool, *CallError) {
	if status < 200 || status >= 300 {
		return nil, false, &CallError{Kind: KindHTTPStatus, Msg: errorMessage(body)}
	}
	if !json.Valid(body) {
		return nil, true, &CallError{Kind: KindBadResponse, Msg: "响应不是 JSON"}
	}
	if isEmptyJSON(body) {
		return nil, true, &CallError{Kind: KindEmpty, Msg: "响应为空"}
	}
	return body, true, nil
}

func isEmptyJSON(b json.RawMessage) bool {
	switch strings.TrimSpace(string(b)) {
	case "", "null", "{}", "[]":
		return true
	}
	return false
}

// errorMessage 从错误响应里挑一句人能看懂的话，只进解析记录。
//
// 认得的形态：FastAPI 的 {"detail": "..."}；TikHub 的 {"detail": {"message_zh": ...,
// "request_id": ...}}（实测 2026-10-16，失败一律不计费）；常见的 message / error。
// TikHub 的请求 ID 一并带上 —— 找它的客服排查时要报这个号。都不认得就截一段原文。
func errorMessage(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if d, ok := m["detail"].(map[string]any); ok {
			if msg := firstString(d, "message_zh", "message"); msg != "" {
				if rid := firstString(d, "request_id"); rid != "" {
					msg += "（request_id " + rid + "）"
				}
				return truncate(msg, 300)
			}
		}
		if msg := firstString(m, "detail", "message_zh", "message", "error"); msg != "" {
			return truncate(msg, 300)
		}
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
