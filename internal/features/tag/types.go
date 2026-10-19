package tag

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("tag not found")
	// ErrIDTaken 是客户端指定的 id 已被别的账号占用（UUIDv4 不会自然撞上）。
	//
	// 同名标签不再是错误（ADR-084，用户决策）：身份只认 UUID，名字只用来显示。
	ErrIDTaken = errors.New("tag id is taken")
)

type Tag struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// SyncSeq 只供增量拉取算游标，不下发（ADR-084）。
	SyncSeq int64 `json:"-"`
}

type CreateInput struct {
	ID   *uuid.UUID `json:"id,omitempty"`
	Name string     `json:"name" binding:"required,min=1,max=50"`
	// CreatedAt 是用户本机建标签的时刻（ADR-084）；在线客户端不带，取服务端时间。
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type UpdateInput struct {
	Name string `json:"name" binding:"required,min=1,max=50"`
}

// PutInput 是一次同步写入：标签的完整状态。以后到达服务端的为准，不核对版本。
type PutInput struct {
	Name      string     `json:"name" binding:"required,min=1,max=50"`
	CreatedAt *time.Time `json:"created_at"`
}

// Outcome 是一次同步写入之后，这个标签在服务端的样子。
type Outcome struct {
	Tag     *Tag // Deleted 时为 nil
	Deleted bool
}
