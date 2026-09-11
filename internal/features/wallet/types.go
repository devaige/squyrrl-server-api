package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/pricing"
)

// GracePeriod 的定义在 pricing 包；这里保留一个别名，让 wallet 的调用点读起来自然。
const GracePeriod = pricing.GracePeriod

// plan 订阅的三种对外状态。
const (
	// PlanStatusNone 没有任何 plan 订阅 —— 免费档。
	PlanStatusNone = "none"
	// PlanStatusActive 正常付费中。
	PlanStatusActive = "active"
	// PlanStatusPastDue 扣款失败但仍在宽限期内，按原档位服务。
	PlanStatusPastDue = "past_due"
)

// PlanState 是「用户现在算哪一档，以及为什么」。
//
// Status 与 GraceUntil 单独暴露，是为了让客户端能提前提醒续费 ——
// 只回一个 tier 的话，用户唯一的信号会是宽限期结束那天功能突然消失。
type PlanState struct {
	Tier   string `json:"tier"`
	Status string `json:"status"`
	// GraceUntil 仅 past_due 时非空：宽限期到此为止，之后按 free 处理。
	GraceUntil *time.Time `json:"grace_until,omitempty"`
}

// ErrInsufficientCredits 由 Consume 在余额 < cost 时返回。
// 上层 handler 应映射到 HTTP 402 Payment Required。
var ErrInsufficientCredits = errors.New("insufficient credits")

// InsufficientCreditsError 在余额不足时携带具体数字，供 handler 构造结构化 402
// （客户端要显示「还差多少」并引导充值）。
//
// Unwrap 到 ErrInsufficientCredits，所以既有的 errors.Is 判断一律不受影响 ——
// 新增的详情是可选信息，不是新的错误类别。
type InsufficientCreditsError struct {
	Balance  int64
	Required int64
}

func (e *InsufficientCreditsError) Error() string {
	return fmt.Sprintf("insufficient credits: have %d, need %d", e.Balance, e.Required)
}

func (e *InsufficientCreditsError) Unwrap() error { return ErrInsufficientCredits }

// ErrNegativeBalance 由 Grant 在扣减会使余额变负时返回。
// 上层 handler 应映射到 HTTP 400 —— 这是请求参数的问题（扣得太多），不是服务端故障。
var ErrNegativeBalance = errors.New("grant would drive balance negative")

// ErrGrantOverflow 由 Grant 在 delta 会让 int64 余额回绕时返回。
// 正常业务量永远碰不到（int64 上限约 9.2e18 credits ≈ $9.2e14），
// 它防的是手滑或恶意填入的极端值 —— 而回绕恰好能穿过负余额检查，所以必须单独挡。
var ErrGrantOverflow = errors.New("grant would overflow balance")

// Wallet 是 /me/wallet 的响应，覆盖三块商品各自的状态（ADR-075）。
//
// 三块必须一起下发：客户端要在同一个界面解释「为什么这个操作被拒了」，
// 只看到其中两块就无法区分 402 是余额不足还是配额已满。
type Wallet struct {
	Plan           string `json:"plan"`            // 'free' / 'basic' / 'standard' / 'premium' / 'maximum'
	CreditsBalance int64  `json:"credits_balance"` // SUM(delta) over credits_ledger，永不过期

	// PlanStatus 与 GraceUntil 说明「这个档位当前是怎么来的」。
	// past_due 时 Plan 仍是原档位（宽限期内照常服务），客户端据此提前提醒续费 ——
	// 不说的话，用户唯一的信号是宽限期结束那天功能突然消失。
	PlanStatus string     `json:"plan_status"`
	GraceUntil *time.Time `json:"grace_until,omitempty"`

	// Limits 是当前档位的完整门槛表，客户端据此在本地预判
	// （比如碎片列表到达上限时提前置灰新建按钮，而不是等服务端回 402）。
	Limits entitlement.Tier `json:"limits"`

	// Usage 是当前占用。与 Limits 成对下发 —— 只有上限而不知道已用了多少，
	// 客户端就算不出「还能再加多少」，而那正是匿名数据迁移界面要实时显示的数字。
	Usage UsageView `json:"usage"`

	// Storage 与 plan 完全无关 —— 它来自独立购买的 storage 订阅，可叠加。
	// 未购买时 quota_bytes 为 0，此时任何文件上传都会被拒。
	Storage StorageView `json:"storage"`
}

// UsageView 是各项资源的当前占用，字段与 Limits 一一对应。
type UsageView struct {
	Snippets int `json:"snippets"`
	Pages    int `json:"pages"`
	Tags     int `json:"tags"`
	Devices  int `json:"devices"`
}

// StorageView 是云存储的配额与占用，字节为单位。
type StorageView struct {
	QuotaBytes int64 `json:"quota_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
}

// GrantInput 由内部端调用（管理后台 / 运营批处理 / 支付回调）
type GrantInput struct {
	UserID uuid.UUID `json:"user_id" binding:"required"`
	Delta  int64     `json:"delta" binding:"required"` // 正数入账，负数也可（罚扣）
	Reason string    `json:"reason" binding:"required"`

	// IdempotencyKey 可选。给定后，同一个键重复提交只会落一笔流水，
	// 后续请求原样返回首次的结果并把 Replayed 置为 true。
	//
	// 支付回调必须传（网关重试是常态而非异常），后台手工发放建议传
	// （客服重复点提交是最常见的重复发放来源）。运营一次性调账可以不传。
	IdempotencyKey string `json:"idempotency_key"`
}

type GrantResponse struct {
	UserID       uuid.UUID `json:"user_id"`
	Delta        int64     `json:"delta"`
	BalanceAfter int64     `json:"balance_after"`
	LedgerID     uuid.UUID `json:"ledger_id"`

	// Replayed 为 true 表示这次请求命中了既有的幂等键，没有产生新流水。
	// 调用方据此区分「刚扣成功」与「之前就扣过了」—— 两者都是成功，但含义不同。
	Replayed bool `json:"replayed,omitempty"`
}
