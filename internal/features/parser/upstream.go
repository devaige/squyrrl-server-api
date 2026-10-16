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

	Request func(uri, resourceID string) extapi.Request
	// Decode 收到的是去掉上游外壳后的数据。返回的结果若没有标题也没有正文，
	// 视同失败、继续试下一个来源（这次调用的钱已经花了）。
	Decode func(data []byte, uri, resourceID string) (*ParseResult, error)
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

// DefaultAdapters 是生产环境注册的全部适配器。
//
// 目前为空：TikHub 各平台要接哪些接口还在逐个确认，每个适配器都要先拿真实响应
// 存进 api_samples 核对字段，再写转换。空目录下所有平台只走内置免费来源。
func DefaultAdapters() AdapterSet {
	return AdapterSet{}
}
