package parser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrNoParser = errors.New("no parser available for this URI")

	// ErrParseFailed 是回给用户的唯一失败原因。具体哪个来源、为什么失败只进解析记录：
	// 报错原文里有上游的名字，而用户不该从任何地方看出解析是谁做的（ADR-080）。
	ErrParseFailed = errors.New("parse failed")
)

// Parser 是一个**平台**：它认得哪些链接归自己、资源 ID 是什么、产物是什么类型。
//
// 怎么拿到内容不在这里 —— 来源有两类，按价格从低到高排成兜底链（ADR-080）：
//   - 内置免费来源：平台自己实现 BuiltinParser（Gist / Reddit 的公开 JSON）；
//   - 付费上游接口：api_endpoints 里的行，每行对应一个 Go 适配器（Adapter）。
//
// oEmbed / Open Graph 不算解析，是客户端的免费「暂存/补全」能力，不进这条链。
type Parser interface {
	// Provider 返回唯一标识，例如 "youtube"。同时作为 parse_cache 的 provider 列、
	// manifest 的 id、解析售价（parse_prices）的键。
	Provider() string

	// SnippetType 决定本平台解析得到的碎片类型：
	//   "special" — 推文 / Gist / YouTube 等专用类型，subtype 用 Provider()
	//   "url"     — 通用链接碎片，无 subtype
	SnippetType() string

	// Match 判断 URI 是否归属本平台；若是，返回该资源在平台命名空间内的稳定 ID。
	// 实现应保证 ID 与该资源生命周期绑定（推文的 numeric ID、视频的 video ID 等）。
	Match(uri string) (resourceID string, ok bool)
}

// BuiltinParser 是可选接口：平台自带一个**免费**来源，排在兜底链最前面。
// 不应自行处理缓存与计费（由 Service 负责）。
type BuiltinParser interface {
	Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error)
}

// ManifestProvider 是可选接口：实现它的 parser 会把匹配正则导出到 GET /uris/manifest，
// 供客户端**本地**判断「这个 URI 服务端能不能解析」，无需每次联网询问。
//
// 清单只列**此刻真有来源**的平台：有内置免费来源，或至少一个启用且有适配器的付费接口
// （见 Service.Manifest）。一个平台的付费接口全部下线后，客户端会把它的链接当普通链接
// 「暂存」，而不是给出一个注定失败的「解析」按钮。
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

	// Version 是上游给出的**内容版本标记**，作为 parse_cache 的第三段键。
	//
	// 只有在来源能真正区分「同一资源的不同内容」时才填（TG 的 edit_date、
	// 带 revision 的 API、兜底可用内容哈希）。留空表示「无版本信息」，
	// 缓存退化为每资源一行 —— 内置的 Gist/Reddit 都在这一档。
	//
	// **不要用 "0" 之类的哨兵冒充「未编辑」**：那会让「上游说了它没被编辑」和
	// 「上游根本没告诉我」挤进同一个值，于是一份新鲜度未知的内容会被当成已确认
	// 是当前版本存下来。拿不到就留空，让它老老实实走 TTL。
	Version string `json:"version,omitempty"`
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

	// Version 回传本次结果对应的内容版本（可能为空，见 ParseResult.Version）。
	//
	// 客户端应把它连同碎片一起存下来。这是「强制刷新后内容到底有没有变」唯一
	// 可判定的依据 —— 服务端答不了这个问题，因为它不知道用户手里那份是哪一版。
	// 有了它，刷新回来 version 相同就可以如实告诉用户「已是最新」，而不是让
	// 用户花了 credits 盯着一个看不出区别的界面猜。
	Version string `json:"version,omitempty"`

	// CreditsCharged 是本次实际扣掉的代币数，即该平台的固定售价（ADR-080）。
	//
	// 售价按平台而异、可在后台调整，客户端无从预知：没有这个字段，钱包余额会在
	// 用户眼里「自己少了一点」，而唯一的解释路径是去翻流水。0 表示未计费（计费关闭）。
	// 解析失败不扣费，也就不会有响应体。
	CreditsCharged int64 `json:"credits_charged"`
}

type ParseInput struct {
	URI string `json:"uri" binding:"required,url"`

	// Force 跳过缓存，强制向上游重取一次。
	//
	// 存在的理由：TTL 是对「这个资源多久会变一次」的猜测，而缓存键指向的是
	// 内容的某一版 —— 猜错时用户会一直看到旧内容，且**没有任何自助手段**。
	// 这是所有 provider 的通用逃生口，不是某个平台的补丁。
	//
	// 计费与普通解析完全一致（同一平台同一价，命中缓存也一样，见 Service.Parse），
	// 所以这个参数不影响价格，只影响拿到的是不是新鲜数据。
	Force bool `json:"force,omitempty"`
}
