package wallet

import (
	"errors"

	"github.com/google/uuid"
)

// ErrInsufficientCredits 由 Consume 在余额 < cost 时返回。
// 上层 handler 应映射到 HTTP 402 Payment Required。
var ErrInsufficientCredits = errors.New("insufficient credits")

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
}

type GrantResponse struct {
	UserID       uuid.UUID `json:"user_id"`
	Delta        int64     `json:"delta"`
	BalanceAfter int64     `json:"balance_after"`
	LedgerID     uuid.UUID `json:"ledger_id"`
}
