package parser

import (
	"sort"

	"github.com/squyrrl/api/internal/features/extapi"
)

// Adapter 把一个付费上游接口转换成我们自己的格式（ADR-080）。
//
// 一个 api_endpoints 行对应一个适配器，按 slug 关联。适配器只做两件事：
// 声明请求（Request），把上游数据转成该平台的 ParseResult（Decode）。
// 鉴权、外壳、计费、兜底都不归它管。
//
// 转换的硬性要求：**产物里不能出现上游的任何痕迹** —— 不写上游名、接口 slug、
// 上游自己的请求 ID。payload / source_data 会进用户的碎片，用户不该从里面看出
// 解析是谁做的；同一平台不管走哪个来源，产出的字段集合应当一致。
type Adapter struct {
	Provider string // 平台，必须是已注册的 Parser.Provider()
	Vendor   string // extapi.VendorTikHub / extapi.VendorRapidAPI，须与接口所属账户一致
	Note     string // 后台展示用：这个接口取的是什么

	// Accept 可选：返回 false 表示这条链接不该交给这个接口（不发请求、不花钱，记为跳过）。
	// 例：小红书的视频笔记接口拿图文笔记 ID 会返回**别的**视频笔记，只接链接里标明
	// type=video 的。nil 表示什么都接。
	Accept func(t Target) bool

	// Shape 是解码用的外壳类型的零值，结构比对据此知道代码读取哪些字段（见 watchedPaths）。
	Shape any

	Request func(t Target) extapi.Request
	// Decode 收到的是去掉上游外壳后的数据。返回的结果若没有标题、正文，也没有图片或视频（见 usable），
	// 视同失败、继续试下一个来源（这次调用的钱已经花了）。
	// 上游可能返回别的作品（推荐流、ID 映射错），能核对作品 ID 的必须核对。
	Decode func(data []byte, t Target) (*ParseResult, error)

	// Resolve 非空时这个接口不取内容，只把分享口令换成作品链接（见 ShareCoder），Decode 为空。
	// 它不进兜底链、不抬售价，也不让平台进解析清单；只决定平台的口令规则下不下发。
	Resolve func(data []byte, t Target) (string, error)
}

// Target 是一次解析的对象。
type Target struct {
	URI        string // 用户提交的原始链接，可能是分享短链；「按分享链接取」的接口用它。提交的是口令时是口令原文
	Resolved   string // 跟完短链跳转、或用口令换来的作品链接；本来就是作品链接时与 URI 相同
	ResourceID string // 平台内的作品 ID，也是缓存键
	Code       string // 提交的是口令时，口令里的码；否则为空
}

// AdapterSet 是按 slug 索引的适配器目录，实现 extapi.Catalog 供后台建接口时校验。
type AdapterSet map[string]Adapter

var _ extapi.Catalog = AdapterSet(nil)

func (s AdapterSet) Adapter(slug string) (extapi.AdapterInfo, bool) {
	a, ok := s[slug]
	if !ok {
		return extapi.AdapterInfo{}, false
	}
	return extapi.AdapterInfo{Slug: slug, Provider: a.Provider, Vendor: a.Vendor, Note: a.Note}, true
}

func (s AdapterSet) Adapters() []extapi.AdapterInfo {
	out := make([]extapi.AdapterInfo, 0, len(s))
	for slug := range s {
		info, _ := s.Adapter(slug)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// callable 判断一个接口此刻有没有能用的代码：适配器存在，且平台、上游都对得上。
// 售价、解析清单、兜底链都以它为准 —— 一个调不通的接口不该抬高售价，也不该让
// 客户端以为这个平台能解析。
func (s AdapterSet) callable(ep extapi.Endpoint) (Adapter, bool) {
	a, ok := s[ep.Slug]
	if !ok || a.Vendor != ep.Vendor || a.Provider != ep.Provider {
		return Adapter{}, false
	}
	return a, true
}

// source 是能取内容的接口（不是换口令的）。
func (s AdapterSet) source(ep extapi.Endpoint) (Adapter, bool) {
	a, ok := s.callable(ep)
	return a, ok && a.Resolve == nil
}

// isResolver 判断一个接口是不是换口令的。没有适配器的接口不算：它在兜底链里照常记一次跳过。
func (s AdapterSet) isResolver(ep extapi.Endpoint) bool {
	a, ok := s[ep.Slug]
	return ok && a.Resolve != nil
}

// DefaultAdapters 是生产环境注册的全部适配器。新增上游时把它的目录并进来。
func DefaultAdapters() AdapterSet {
	return TikHubAdapters()
}
