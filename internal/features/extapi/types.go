package extapi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNoEndpoint 表示该 provider 当前没有可用（enabled + 有余额 + 配置完整）的付费 endpoint。
// 上层据此回退到内置免费实现（如 YouTube oEmbed）。
var ErrNoEndpoint = errors.New("no external api endpoint available for provider")

// Endpoint 是一个外部第三方解析 API 的接入点。同一 provider 下多个 endpoint 按 priority 构成兜底链。
type Endpoint struct {
	ID              uuid.UUID       `json:"id"`
	Provider        string          `json:"provider"`
	Vendor          string          `json:"vendor"`
	Slug            string          `json:"slug"`
	Priority        int             `json:"priority"`
	Enabled         bool            `json:"enabled"`
	UnitPriceMicros int64           `json:"unit_price_micros"`
	Currency        string          `json:"currency"`
	Config          json.RawMessage `json:"config"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`

	// 以下为聚合字段，仅列表/详情查询填充（Fetch 路径只用到 BalanceMicros）
	BalanceMicros int64 `json:"balance_micros"`
	SpentMicros   int64 `json:"spent_micros"`
	CallCount     int64 `json:"call_count"`
}

// EndpointConfig 是 config JSONB 的强类型形态，驱动据此构造请求、映射响应。
//
// 占位符在 url_template / query / headers / body 中生效：
//
//	${resource_id}  被解析资源在 provider 命名空间内的 ID（视频 ID 等）
//	${uri}          原始 URI
//	${env:VARNAME}  运行时读环境变量（用于 API key —— 密钥永远只在 env，不落库）
type EndpointConfig struct {
	Method      string            `json:"method"`       // 默认 GET
	URLTemplate string            `json:"url_template"` // 必填；为空视为「无驱动配置」，Fetch 跳过
	Headers     map[string]string `json:"headers"`
	Query       map[string]string `json:"query"`
	Body        string            `json:"body"`
	ResponseMap ResponseMap       `json:"response_map"`
	ArchiveLive bool              `json:"archive_live"` // 是否把线上真实调用采样进 api_samples
	TimeoutMs   int               `json:"timeout_ms"`   // 默认 8000
}

// ResponseMap 用「点路径」把上游任意 JSON 响应映射到通用字段。
// 路径支持对象键与数组下标，如 "items.0.snippet.title"。
type ResponseMap struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	ThumbnailURL string `json:"thumbnail_url"`
	AuthorName   string `json:"author_name"`
	AuthorURL    string `json:"author_url"`
}

// FetchResult 是供应层对上层暴露的中立结果；由 parser 侧组装成 provider-specific 的 snippet。
type FetchResult struct {
	Title        string
	Description  string
	ThumbnailURL string
	AuthorName   string
	AuthorURL    string
	Raw          json.RawMessage // 原始响应体，parser 需要额外字段时可自取
	EndpointID   uuid.UUID
	EndpointSlug string
}

// ---- 管理后台入参 ----

type CreateEndpointInput struct {
	Provider        string          `json:"provider" binding:"required"`
	Vendor          string          `json:"vendor" binding:"required"`
	Slug            string          `json:"slug" binding:"required"`
	Priority        int             `json:"priority"`
	Enabled         *bool           `json:"enabled"`
	UnitPriceMicros int64           `json:"unit_price_micros"`
	Currency        string          `json:"currency"`
	Config          json.RawMessage `json:"config"`
}

type UpdateEndpointInput struct {
	Vendor          *string          `json:"vendor"`
	Priority        *int             `json:"priority"`
	Enabled         *bool            `json:"enabled"`
	UnitPriceMicros *int64           `json:"unit_price_micros"`
	Currency        *string          `json:"currency"`
	Config          *json.RawMessage `json:"config"`
}

type TopupInput struct {
	AmountMicros int64  `json:"amount_micros" binding:"required"` // 正=充值/额度录入，负=手工核减
	Reason       string `json:"reason"`
}

type CreateSampleInput struct {
	ResourceID string          `json:"resource_id"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	HTTPStatus int             `json:"http_status"`
	Note       string          `json:"note"`
}

// Sample 是一条请求/响应存档记录。
type Sample struct {
	ID         uuid.UUID       `json:"id"`
	EndpointID uuid.UUID       `json:"endpoint_id"`
	Kind       string          `json:"kind"` // 'sample' | 'live'
	ResourceID string          `json:"resource_id"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	HTTPStatus int             `json:"http_status"`
	LatencyMs  int             `json:"latency_ms"`
	Note       string          `json:"note"`
	CreatedAt  time.Time       `json:"created_at"`
}
