package parser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// ManifestProvider 是可选接口：实现它的 parser 会把匹配正则导出到 GET /uris/manifest，
// 供客户端**本地**判断「这个 URI 服务端能不能解析」，无需每次联网询问。
//
// 关键：通用 OG 兜底（GenericOGProvider）**故意不实现**它——它匹配任意 http(s) URL，
// 那是「暂存/enrich」路径而非「支持解析」。清单只声明专用 provider 的能力。
//
// 单一真源：MatchPatterns 直接从各 provider 已有的正则常量 .String() 派生，
// 与 Match 用的是同一份正则，服务端改正则→导出自动跟着变，客户端判断永不与服务端脱节。
type ManifestProvider interface {
	MatchPatterns() []string
}

// ManifestEntry 是单个 provider 导出的匹配规则。刻意极简（只 id + patterns）：
// 客户端只需 yes/no 归属判断；resource_id 的抽取仍是服务端 Parse 时的内部事，不外泄。
type ManifestEntry struct {
	ID       string   `json:"id"`
	Patterns []string `json:"patterns"`
}

// Manifest 是导出的「受支持解析」清单。Version 用内容哈希而非手工计数器：
// 只随 entries 变化而变，客户端拿它与本地缓存比对即可决定是否采纳新拉取的版本，
// 既不会「改了正则忘了 bump」，也不会漂移。
type Manifest struct {
	Version   string          `json:"version"`
	Providers []ManifestEntry `json:"providers"`
}

// Asset 是解析产物里「可下载的远端文件」的 provider 无关声明。
//
// 关键设计（ADR-062）：文件地址原本埋在各 provider 私有的 payload 键里
// （youtube 的 thumbnail_url、OG 的 og_image……），客户端无从通用发现。
// 这里把它们统一提出来成一个列表，客户端只认 Assets：逐个下载→上传拿 file_id
// →挂成 snippet.elements。**服务端不下载、不代传**，仅声明地址与语义。
// payload 里的原始地址保留不动（供富渲染/兜底），Assets 是并存的「下载指令」。
type Asset struct {
	URL  string `json:"url"`
	Role string `json:"role,omitempty"` // thumbnail | image | media —— 客户端排序/alt 的语义提示
	Mime string `json:"mime,omitempty"` // 尽力而为的提示；客户端下载后仍会按响应/字节自行判定
	Alt  string `json:"alt,omitempty"`
}

// ParseResult 是 Parser 输出的中间形态，由 Service 拼装为 ParsedSnippet 后返回客户端。
type ParseResult struct {
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload"`     // type-specific 数据
	SourceData  json.RawMessage `json:"source_data"` // upstream author 信息
	Assets      []Asset         `json:"assets,omitempty"`
}

// ParsedSnippet 是解析产物的**原始/中间形态**，不是可直接入库的成品碎片。
//
// 客户端拿到后需要：下载 Assets 里的文件 → 上传拿 file_id → 组装 elements →
// 连同 payload/subtype 走碎片创建入口落库（ADR-062）。**解析层绝不直接 POST /snippets**。
// payload 承载「比普通碎片更多」的 subtype 富数据，驱动 special 卡片专属渲染；
// 碎片被编辑保存时坍塌为 normal——payload 被丢弃，下载好的文件仍留在 elements 中存活。
type ParsedSnippet struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	SourceType  string          `json:"source_type"`
	SourceData  json.RawMessage `json:"source_data"`
	Assets      []Asset         `json:"assets,omitempty"`
}

// mediaAsset 仅在 rawURL 是 http(s) 绝对地址时返回单元素资产切片，否则返回 nil。
// 各 provider 的缩略图字段常是占位串（reddit 的 "self"/"default"/"nsfw"）或空串，
// 直接过滤掉，避免客户端拿去下载一个非 URL。用 append(existing, mediaAsset(...)...)
// 组合多资产时，nil 会被 append 自然吞掉。
func mediaAsset(rawURL, role, mime string) []Asset {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil
	}
	return []Asset{{URL: rawURL, Role: role, Mime: mime}}
}

type ParseResponse struct {
	Provider   string         `json:"provider"`
	ResourceID string         `json:"resource_id"`
	Snippet    *ParsedSnippet `json:"snippet"`
	Cached     bool           `json:"cached"`

	// CreditsCharged 是本次实际扣掉的代币数。
	//
	// 必须回传，因为价格自 2026-09-12 起随 provider 接的上游而变，客户端无从预知：
	// 没有这个字段，钱包余额会在用户眼里「自己少了一点」，而唯一的解释路径是
	// 去翻流水。0 表示未计费（计费关闭，或走的是余额不足以外的免单路径）。
	CreditsCharged int64 `json:"credits_charged"`
}

type ParseInput struct {
	URI string `json:"uri" binding:"required,url"`
}
