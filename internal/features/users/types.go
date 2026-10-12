package users

import (
	"time"

	"github.com/google/uuid"
)

// 搜索结果条数。每行要额外查一次钱包（见 Service.Search），上限因此压得比普通列表低。
const (
	DefaultLimit = 20
	MaxLimit     = 50
)

// Summary 是管理后台用户列表的一行：账户本身 + 运营最常问的两项钱包状态。
type Summary struct {
	ID        uuid.UUID `json:"id"`
	Email     *string   `json:"email,omitempty"`
	Handle    *string   `json:"handle,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Plan / PlanStatus / GraceUntil 与 /me/wallet 同义，宽限期已算在内。
	Plan           string     `json:"plan"`
	PlanStatus     string     `json:"plan_status"`
	GraceUntil     *time.Time `json:"grace_until,omitempty"`
	CreditsBalance int64      `json:"credits_balance"`
}

// clampLimit 把调用方给的条数收进 [1, MaxLimit]；不传或非正数时取默认值。
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}
