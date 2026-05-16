package snippet

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound        = errors.New("snippet not found")
	ErrVersionConflict = errors.New("version conflict")
	ErrTagNotOwned     = errors.New("one or more tag_ids do not belong to user")
	ErrPageNotOwned    = errors.New("page_id does not belong to user")
	ErrFileNotFound    = errors.New("one or more file_ids do not exist")
)

// ConflictError 在 PATCH 触发版本冲突时由 Service 返回。
// errors.Is(err, ErrVersionConflict) 仍然为 true，便于上层简化判断；
// 而 errors.As 可拿到 archived loser 副本的 ID 给客户端定位。
type ConflictError struct {
	LoserSnippetID uuid.UUID
	LoserPageID    uuid.UUID
	CurrentVersion int64
}

func (e *ConflictError) Error() string             { return "version conflict" }
func (e *ConflictError) Is(target error) bool      { return target == ErrVersionConflict }

type Element struct {
	ID        uuid.UUID `json:"id"`
	FileID    uuid.UUID `json:"file_id"`
	OrderIdx  int       `json:"order_idx"`
	AltText   *string   `json:"alt_text,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ElementInput struct {
	FileID   uuid.UUID `json:"file_id" binding:"required"`
	OrderIdx int       `json:"order_idx"`
	AltText  *string   `json:"alt_text,omitempty"`
}

// Snippet 是返回给客户端的完整快照。text_content 在 JSON 中以 base64 出现（Go json 的默认 []byte 编码）。
// 服务器永远不解密 text_content；它仅作为不透明字节流存取。
type Snippet struct {
	ID          uuid.UUID       `json:"id"`
	PageID      *uuid.UUID      `json:"page_id"`
	Type        string          `json:"type"`
	Subtype     *string         `json:"subtype,omitempty"`
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	TextContent []byte          `json:"text_content,omitempty"`
	TextFormat  string          `json:"text_format"`
	TextLang    *string         `json:"text_lang,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	SourceType  string          `json:"source_type"`
	SourceData  json.RawMessage `json:"source_data"`
	Version     int64           `json:"version"`
	TagIDs      []uuid.UUID     `json:"tag_ids"`
	Elements    []Element       `json:"elements"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DeletedAt   *time.Time      `json:"deleted_at,omitempty"`
}

type CreateInput struct {
	ID          *uuid.UUID      `json:"id,omitempty"`
	PageID      *uuid.UUID      `json:"page_id,omitempty"`
	Type        string          `json:"type" binding:"required,oneof=uri url normal special"`
	Subtype     *string         `json:"subtype,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	TextContent []byte          `json:"text_content,omitempty"`
	TextFormat  string          `json:"text_format,omitempty" binding:"omitempty,oneof=plain markdown code"`
	TextLang    *string         `json:"text_lang,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	SourceType  string          `json:"source_type" binding:"required,oneof=device upstream"`
	SourceData  json.RawMessage `json:"source_data,omitempty"`
	TagIDs      []uuid.UUID     `json:"tag_ids,omitempty"`
	Elements    []ElementInput  `json:"elements,omitempty"`
}

// UpdateInput 采用「字段存在即更新」语义。version 字段做乐观锁。
// 限制：本接口暂不支持把 page_id / title / description / text_lang 显式置空（pointer 无法区分 omit 与 null）。
//      若需清空，使用专用端点（待后续实现）或重建碎片。
type UpdateInput struct {
	Version int64 `json:"version" binding:"required"`

	PageID      *uuid.UUID      `json:"page_id,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	TextContent []byte          `json:"text_content,omitempty"`
	TextFormat  *string         `json:"text_format,omitempty" binding:"omitempty,oneof=plain markdown code"`
	TextLang    *string         `json:"text_lang,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	TagIDs      *[]uuid.UUID    `json:"tag_ids,omitempty"` // nil = 不变；空切片 = 清空所有标签
	Elements    *[]ElementInput `json:"elements,omitempty"` // nil = 不变；空切片 = 清空所有元素
}

type ListInput struct {
	// PageID / TagID 不通过 Gin form binding —— Gin v1.12 对 *uuid.UUID query 解析有 quirk
	// （把单值包成 ["..."] 再 stringify）。handler 显式调 uuid.Parse 填充。
	PageID         *uuid.UUID `form:"-"`
	TagID          *uuid.UUID `form:"-"`
	Since          *time.Time `form:"since"`           // RFC3339；返回 updated_at >= since
	IncludeDeleted bool       `form:"include_deleted"` // 同步场景需要
	Limit          int        `form:"limit"`
	// q：全文搜索关键字。Phase 1.5 仅对 title / description 做 ILIKE 单词包含匹配；
	// text_content 因将来要走客户端混淆，不在服务端索引。
	Q *string `form:"q"`
}

type ListOutput struct {
	Items     []Snippet  `json:"items"`
	NextSince *time.Time `json:"next_since,omitempty"`
}
