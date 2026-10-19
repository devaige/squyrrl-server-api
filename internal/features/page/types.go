package page

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound       = errors.New("page not found")
	ErrSystemReadOnly = errors.New("system page is read-only")
	// ErrIDTaken 是客户端指定的 id 已被别的账号占用（UUIDv4 不会自然撞上）。
	ErrIDTaken = errors.New("page id is taken")
)

type Page struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	IsHidden  bool      `json:"is_hidden"`
	IsSystem  bool      `json:"is_system"`
	OrderIdx  int       `json:"order_idx"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// SyncSeq 只供增量拉取算游标，不下发（ADR-084）。
	SyncSeq int64 `json:"-"`
}

type CreateInput struct {
	ID       *uuid.UUID `json:"id,omitempty"` // 客户端可指定（离线生成）
	Name     string     `json:"name" binding:"required,min=1,max=100"`
	IsHidden bool       `json:"is_hidden"`
	OrderIdx int        `json:"order_idx"`
	// CreatedAt 是用户本机建页面的时刻（ADR-084）；在线客户端不带，取服务端时间。
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type UpdateInput struct {
	Name     *string `json:"name,omitempty"     binding:"omitempty,min=1,max=100"`
	IsHidden *bool   `json:"is_hidden,omitempty"`
	OrderIdx *int    `json:"order_idx,omitempty"`
}

// PutInput 是一次同步写入：页面的完整状态。以后到达服务端的为准，不核对版本。
type PutInput struct {
	Name      string     `json:"name" binding:"required,min=1,max=100"`
	IsHidden  bool       `json:"is_hidden"`
	OrderIdx  int        `json:"order_idx"`
	CreatedAt *time.Time `json:"created_at"`
}
