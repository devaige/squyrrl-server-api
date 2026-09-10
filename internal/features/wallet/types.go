package wallet

import (
	"errors"

	"github.com/google/uuid"
)

// ErrInsufficientCredits 由 Consume 在余额 < cost 时返回。
// 上层 handler 应映射到 HTTP 402 Payment Required。
var ErrInsufficientCredits = errors.New("insufficient credits")

// ErrNegativeBalance 由 Grant 在扣减会使余额变负时返回。
// 上层 handler 应映射到 HTTP 400 —— 这是请求参数的问题（扣得太多），不是服务端故障。
var ErrNegativeBalance = errors.New("grant would drive balance negative")

// ErrGrantOverflow 由 Grant 在 delta 会让 int64 余额回绕时返回。
// 正常业务量永远碰不到（int64 上限约 9.2e18 credits ≈ $9.2e14），
// 它防的是手滑或恶意填入的极端值 —— 而回绕恰好能穿过负余额检查，所以必须单独挡。
var ErrGrantOverflow = errors.New("grant would overflow balance")

// Wallet 是 /me/wallet 端点的响应：当前活跃 plan + 代币余额
type Wallet struct {
	Plan           string `json:"plan"`            // 'free' / 'basic' / 'standard' / 'premium' / 'maximum'
	CreditsBalance int64  `json:"credits_balance"` // SUM(delta) over credits_ledger
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
