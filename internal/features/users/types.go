package users

import (
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/wallet"
)

// 搜索结果条数。每行要额外查钱包、存储与订阅（见 Service.Search），上限因此压得比普通列表低。
const (
	DefaultLimit = 20
	MaxLimit     = 50
)

// Summary 是管理后台用户列表的一行：账户本身 + 档位、代币、存储三块状态。
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

	// Storage 与 /me/wallet 的 storage 同源（quota.Service.Storage）。
	Storage wallet.StorageView `json:"storage"`

	// Subscriptions 是仍在服务的订阅行：后台据此区分档位是付费来的还是手动给的，
	// 并逐条列出存储档位（存储可叠加，一个用户可能同时有好几份）。
	Subscriptions []subscriptions.Record `json:"subscriptions"`
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
