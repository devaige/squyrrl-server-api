package page

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound       = errors.New("page not found")
	ErrSystemReadOnly = errors.New("system page is read-only")
)

type Page struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	IsHidden  bool      `json:"is_hidden"`
	IsSystem  bool      `json:"is_system"`
	OrderIdx  int       `json:"order_idx"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateInput struct {
	ID       *uuid.UUID `json:"id,omitempty"`                                    // 客户端可指定（离线生成）
	Name     string     `json:"name" binding:"required,min=1,max=100"`
	IsHidden bool       `json:"is_hidden"`
	OrderIdx int        `json:"order_idx"`
}

type UpdateInput struct {
	Name     *string `json:"name,omitempty"     binding:"omitempty,min=1,max=100"`
	IsHidden *bool   `json:"is_hidden,omitempty"`
	OrderIdx *int    `json:"order_idx,omitempty"`
}
