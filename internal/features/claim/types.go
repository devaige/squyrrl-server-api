// Package claim 实现 ADR-051 的匿名数据归属端点。
// 用户在未登录态产生的本地数据（pages / tags / snippets / elements 引用）
// 在登录成功后一次性 POST /me/anonymous/claim 写入账户名下。
package claim

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrPayloadTooLarge   = errors.New("claim payload exceeds size limits")
	ErrFileNotFound      = errors.New("one or more referenced file_ids do not exist")
	ErrInvalidPageRef    = errors.New("snippet.page_id references a page not in the claim and not owned by user")
	ErrDuplicateName     = errors.New("tag name collision could not be resolved")
)

// 上限：单次 claim 单次扁平最多多少条；超过返回 ErrPayloadTooLarge。
// 防止恶意客户端通过单一 claim 撑爆账户。可由配置覆盖（暂未走 cfg，硬编码）。
const (
	MaxPages    = 1_000
	MaxTags     = 1_000
	MaxSnippets = 10_000
	MaxElements = 50_000
)

// PageInput 与 page.CreateInput 类似，但 id 必填（客户端在离线创建时已生成 UUID）。
type PageInput struct {
	ID       uuid.UUID `json:"id" binding:"required"`
	Name     string    `json:"name" binding:"required,min=1,max=100"`
	IsHidden bool      `json:"is_hidden"`
	OrderIdx int       `json:"order_idx"`
}

type TagInput struct {
	ID   uuid.UUID `json:"id" binding:"required"`
	Name string    `json:"name" binding:"required,min=1,max=50"`
}

type ElementInput struct {
	FileID   uuid.UUID `json:"file_id" binding:"required"`
	OrderIdx int       `json:"order_idx"`
	AltText  *string   `json:"alt_text,omitempty"`
}

// SnippetInput 是 snippet.CreateInput 的批量版本：id 必填、保留客户端 UUID；
// tag_ids 中的 UUID 对应同次 claim 的 TagInput.ID 或用户已有 tag。
// 服务端会按需 remap 到最终落库的 tag UUID（应对 tag name 冲突）。
type SnippetInput struct {
	ID          uuid.UUID       `json:"id" binding:"required"`
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
	CreatedAt   *time.Time      `json:"created_at,omitempty"`
}

// ClaimInput 是 POST /me/anonymous/claim 的请求体。
// ClientDedupeKey 用于幂等：同一 key 重试返回首次的写入结果。
type ClaimInput struct {
	ClientDedupeKey uuid.UUID      `json:"client_dedupe_key" binding:"required"`
	Pages           []PageInput    `json:"pages,omitempty"`
	Tags            []TagInput     `json:"tags,omitempty"`
	Snippets        []SnippetInput `json:"snippets,omitempty"`
}

// ClaimResult 是 claim 端点的响应；幂等重放亦返回同一 result。
type ClaimResult struct {
	ClientDedupeKey uuid.UUID `json:"client_dedupe_key"`
	UserID          uuid.UUID `json:"user_id"`
	SnippetCount    int       `json:"snippet_count"`
	PageCount       int       `json:"page_count"`
	TagCount        int       `json:"tag_count"`
	ElementCount    int       `json:"element_count"`
	Idempotent      bool      `json:"idempotent"` // 是否是幂等重放（true = 早前已 claim 过同一 key）
}
