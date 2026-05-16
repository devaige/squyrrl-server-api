package parser

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNoParser = errors.New("no parser available for this URI")
)

// Parser 是 URI 解析适配器接口。每种来源（YouTube / Tweet / Gist / ...）实现一份。
//
// Phase 1.5 已实现：YouTube oEmbed
// Phase 2 计划：Twitter/X、GitHub Gist、通用 Open Graph 兜底
type Parser interface {
	// Provider 返回唯一标识，例如 "youtube"。同时作为 parse_cache 的 provider 列。
	Provider() string

	// SnippetType 决定本 provider 解析得到的碎片类型：
	//   "special" — 推文 / Gist / YouTube 等专用类型，subtype 用 Provider()
	//   "url"     — 通用 OG 兜底解析得到的链接碎片，无 subtype
	SnippetType() string

	// Match 判断 URI 是否归属本 parser；若是，返回该资源在 provider 命名空间内的稳定 ID。
	// 实现应保证 ID 与该资源生命周期绑定（推文的 numeric ID、视频的 video ID 等）。
	Match(uri string) (resourceID string, ok bool)

	// Parse 调用上游服务并返回结构化数据；不应自行处理缓存（由 Service 负责）。
	Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error)
}

// ParseResult 是 Parser 输出的中间形态，由 Service 拼装为 ParsedSnippet 后返回客户端。
type ParseResult struct {
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload"`     // type-specific 数据
	SourceData  json.RawMessage `json:"source_data"` // upstream author 信息
}

// ParsedSnippet 直接对接 POST /snippets 的字段命名；客户端可直接转发或编辑后落库。
type ParsedSnippet struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	SourceType  string          `json:"source_type"`
	SourceData  json.RawMessage `json:"source_data"`
}

type ParseResponse struct {
	Provider   string         `json:"provider"`
	ResourceID string         `json:"resource_id"`
	Snippet    *ParsedSnippet `json:"snippet"`
	Cached     bool           `json:"cached"`
}

type ParseInput struct {
	URI string `json:"uri" binding:"required,url"`
}
