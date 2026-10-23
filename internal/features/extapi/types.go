package extapi

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// extapi 是付费解析上游的供应层（ADR-056，ADR-080 重构）：
//
//	账户 Account  —— 钱实际存放、被扣减的地方。直连上游（TikHub）一个账户；
//	                 聚合平台（RapidAPI）每个子提供商一个账户。
//	接口 Endpoint —— 某个账户下、某个平台的某个功能，一行对应一个 Go 适配器（按 slug）。
//
// 本包只管目录、记账、鉴权与 HTTP 调用；「请求怎么拼、响应怎么转成我们自己的格式」
// 在 parser 包的适配器里 —— 那需要 parser 的类型，而 parser 依赖本包。

var (
	ErrUnknownVendor  = errors.New("unknown upstream vendor")
	ErrUnknownAdapter = errors.New("no adapter registered for this slug")
	ErrVendorMismatch = errors.New("adapter vendor does not match account vendor")
)

// Account 是一个计费账户。余额 = 充值/调账 − 调用成本，全部来自 api_cost_ledger。
type Account struct {
	ID        uuid.UUID `json:"id"`
	Vendor    string    `json:"vendor"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Currency  string    `json:"currency"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	BalanceMicros  int64 `json:"balance_micros"`
	ToppedUpMicros int64 `json:"topped_up_micros"` // 充值与调账（delta>0）累计
	SpentMicros    int64 `json:"spent_micros"`     // 调用成本与手工核减（delta<0）累计
	CallCount      int64 `json:"call_count"`       // 计费调用次数
}

// Endpoint 是一个可调用的上游接口。同一平台下的多个接口按单价升序构成兜底链。
type Endpoint struct {
	ID              uuid.UUID       `json:"id"`
	AccountID       uuid.UUID       `json:"account_id"`
	Provider        string          `json:"provider"` // 平台，对应 parser.Parser.Provider()
	Slug            string          `json:"slug"`     // 同时是适配器键（parser.Adapter）
	Priority        int             `json:"priority"` // 同价时的先后，越小越先
	Enabled         bool            `json:"enabled"`
	UnitPriceMicros int64           `json:"unit_price_micros"` // 每次被上游计费的调用成本，币种百万分之一
	Config          json.RawMessage `json:"config"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`

	// 以下来自所属账户
	Vendor        string `json:"vendor"`
	AccountSlug   string `json:"account_slug"`
	AccountName   string `json:"account_name"`
	Currency      string `json:"currency"`
	BalanceMicros int64  `json:"balance_micros"` // 账户余额，同账户的接口共用

	// 以下为接口自身的统计，仅后台列表填充
	CallCount   int64 `json:"call_count"`
	SpentMicros int64 `json:"spent_micros"`
}

// EndpointConfig 是 config JSONB 的强类型形态。只剩运行参数 ——
// 请求与响应映射在适配器代码里。
type EndpointConfig struct {
	TimeoutMs   int  `json:"timeout_ms"`   // 默认 15000
	ArchiveLive bool `json:"archive_live"` // 把线上真实调用采样进 api_samples，用来核对适配器
}

// LedgerEntry 是一笔账户流水。
type LedgerEntry struct {
	ID         uuid.UUID  `json:"id"`
	AccountID  uuid.UUID  `json:"account_id"`
	EndpointID *uuid.UUID `json:"endpoint_id,omitempty"`
	Delta      int64      `json:"delta"`  // micros；正=充值，负=调用成本/核减
	Reason     string     `json:"reason"` // topup / adjust / call
	Note       string     `json:"note"`
	ResourceID string     `json:"resource_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// AdapterInfo 描述一个已在代码里注册的适配器，供后台建接口时选择。
type AdapterInfo struct {
	Slug     string `json:"slug"`
	Provider string `json:"provider"`
	Vendor   string `json:"vendor"`
	Note     string `json:"note"`
}

// Catalog 是适配器目录。由 parser 实现、在装配时注入 ——
// 本包据此校验「建的接口有没有对应代码」「挂的账户是不是同一上游」。
type Catalog interface {
	Adapter(slug string) (AdapterInfo, bool)
	Adapters() []AdapterInfo
	// WatchedPaths 是该接口的适配器代码会读取的字段路径，结构比对只拿这些算缺失；
	// 为空时退回比对全部字段。
	WatchedPaths(slug string) []string
	// Examples 是该接口的示例，线上采样的结构比对以它们为基准。
	Examples(slug string) []Example
}

// ---- 管理后台入参 ----

type CreateAccountInput struct {
	Vendor   string `json:"vendor" binding:"required"`
	Slug     string `json:"slug" binding:"required"`
	Name     string `json:"name" binding:"required"`
	Currency string `json:"currency"`
	Note     string `json:"note"`
}

type UpdateAccountInput struct {
	Name *string `json:"name"`
	Note *string `json:"note"`
}

// TopupInput 录入一笔充值（正数）或手工核减（负数）。
type TopupInput struct {
	AmountMicros int64  `json:"amount_micros" binding:"required"`
	Note         string `json:"note"`
}

type CreateEndpointInput struct {
	Slug            string          `json:"slug" binding:"required"` // 必须是已注册的适配器
	AccountID       uuid.UUID       `json:"account_id" binding:"required"`
	Priority        int             `json:"priority"`
	Enabled         *bool           `json:"enabled"`
	UnitPriceMicros int64           `json:"unit_price_micros"`
	Config          json.RawMessage `json:"config"`
}

type UpdateEndpointInput struct {
	Priority        *int             `json:"priority"`
	Enabled         *bool            `json:"enabled"`
	UnitPriceMicros *int64           `json:"unit_price_micros"`
	Config          *json.RawMessage `json:"config"`
}

// Example 是一份「示例」：接入或补全适配器时核对过的一种响应结构（如图片推文、视频推文）。
// 实测响应在代码里维护（parser/testdata/tikhub/），随部署生效；后台只读，发现新结构由开发
// 补进代码（用户决定 2026-10-19）。原先示例存在 api_samples 里、可在后台存为示例，但内置
// 示例只在接口一份都没有时写一次，之后补进代码的形态到不了线上库。
type Example struct {
	Note       string          `json:"note"` // 是哪种结构
	ResourceID string          `json:"resource_id"`
	Data       json.RawMessage `json:"-"` // 业务数据本身，即 TikHub 外壳里的 data

	// 仅接口的采样列表：和同接口其他每份示例的结构差异
	Versus []ExampleDiff `json:"versus,omitempty"`
}

// Sample 是一条线上采样：archive_live 打开时每次调用自动存一条请求与原始响应，保留最近
// LiveSampleRetention、每接口最多 LiveSamplesPerEndpoint 条；解析记录里每一步都能点回当次的这一条。
type Sample struct {
	ID         uuid.UUID       `json:"id"`
	EndpointID uuid.UUID       `json:"endpoint_id"`
	ResourceID string          `json:"resource_id"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	HTTPStatus int             `json:"http_status"`
	LatencyMs  int             `json:"latency_ms"`
	CreatedAt  time.Time       `json:"created_at"`
	ViewedAt   *time.Time      `json:"viewed_at,omitempty"` // 后台第一次打开详情的时间

	// 相对于该接口示例的结构差异（列表里只有计数，详情里带字段清单）；上游失败的应答没有
	Diff *StructureDiff `json:"diff,omitempty"`
	// 仅详情：所属接口
	EndpointSlug string `json:"endpoint_slug,omitempty"`
	Provider     string `json:"provider,omitempty"`
}

// Drift 是一条线上采样相对示例的差异计数，以及后台有没有打开过它的详情。
// 解析记录列表靠它标出还没看过的结构变化。
type Drift struct {
	MissingCount int  `json:"missing_count"`
	AddedCount   int  `json:"added_count"`
	Viewed       bool `json:"viewed"`
}
