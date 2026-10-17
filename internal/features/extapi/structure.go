package extapi

import (
	"encoding/json"
	"sort"
	"strings"
)

// 结构比对：上游返回的 data 没有契约，会随内容形态（图文 / 视频 / 动图……）和上游改版变化。
// 「示例」是我们核对过的请求与响应，每发现一种新结构就存一份（用户决定 2026-10-16）。
// 线上采样拿来和示例逐字段比，只比字段有没有、不比值：
//
//	缺失 = 适配器代码会读、所有示例都有、这一份却没有的字段 —— 多半是上游改了结构，
//	       或者是一种还没存过的新形态；
//	新增 = 所有示例都没有、这一份有的字段 —— 只作提示，可能是新形态。
//
// 缺失要同时满足两个条件才算，都是为了压掉噪音：
//   - 「所有示例都有」：结构本来就随内容形态变，视频推文有 media.video、图片推文没有；
//   - 「代码会读」：大量字段随内容可有可无（直播间、背景音乐、各语言字幕），实测同一接口
//     两条正常响应差出 20 多个这类字段，而代码不读的字段缺了不影响解析。

const (
	maxShapeDepth = 12
	maxShapePaths = 20000
	maxDiffList   = 100
)

// StructureDiff 是一份响应相对于示例的结构差异。
type StructureDiff struct {
	Baseline     int      `json:"baseline"`      // 参与比对的示例数；0 表示还没有示例，无从比较
	MissingCount int      `json:"missing_count"` // 按分支计：缺了 author 只算一处，不再数 author 底下的子键
	AddedCount   int      `json:"added_count"`
	Missing      []string `json:"missing,omitempty"` // 最多 maxDiffList 条，计数不受这个截断影响
	Added        []string `json:"added,omitempty"`
}

// diffable 判断一条存档要不要和示例比：只比线上采样里上游正常应答（2xx）的那些。
// 上游明说失败的应答（TikHub 的 400「本次请求不会被扣费」）是一段错误说明，拿去比会报出
// 一长串「缺失」，看起来像上游改了结构，其实只是这次没取到。
func diffable(x Sample) bool {
	return x.Kind == "live" && x.HTTPStatus >= 200 && x.HTTPStatus < 300
}

// payloadOf 从存档里取出要比的部分：存档是 {"status", "body"}，TikHub 的业务数据在 body.data，
// 外壳（request_id、time、cache_url……）每次都一样，比它没有意义。
func payloadOf(response json.RawMessage) any {
	var arch struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(response, &arch) != nil || len(arch.Body) == 0 {
		return nil
	}
	var body any
	if json.Unmarshal(arch.Body, &body) != nil {
		return nil
	}
	if m, ok := body.(map[string]any); ok {
		if data, ok := m["data"]; ok {
			if _, hasCode := m["code"]; hasCode {
				return data
			}
		}
	}
	return body
}

// shapeOf 收集 JSON 里所有键的路径，数组下标折叠成 []（各元素取并集），值为 null 的键也算存在。
func shapeOf(v any) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(v any, prefix string, depth int)
	walk = func(v any, prefix string, depth int) {
		if depth > maxShapeDepth || len(out) >= maxShapePaths {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out[p] = struct{}{}
				walk(child, p, depth+1)
			}
		case []any:
			for _, child := range x {
				walk(child, prefix+"[]", depth+1)
			}
		}
	}
	walk(v, "", 0)
	return out
}

// diffAgainst 把一份响应和一组示例比。watched 为空时缺失按全部字段算。
func diffAgainst(response json.RawMessage, baselines []json.RawMessage, watched []string, withLists bool) *StructureDiff {
	d := &StructureDiff{Baseline: len(baselines)}
	if len(baselines) == 0 {
		return d
	}
	live := shapeOf(payloadOf(response))

	var core map[string]struct{} // 所有示例共有
	union := map[string]struct{}{}
	for _, b := range baselines {
		s := shapeOf(payloadOf(b))
		for p := range s {
			union[p] = struct{}{}
		}
		if core == nil {
			core = s
			continue
		}
		for p := range core {
			if _, ok := s[p]; !ok {
				delete(core, p)
			}
		}
	}

	if len(watched) > 0 {
		keep := make(map[string]struct{}, len(watched))
		for _, p := range watched {
			keep[p] = struct{}{}
		}
		for p := range core {
			if _, ok := keep[p]; !ok {
				delete(core, p)
			}
		}
	}

	var missing, added []string
	for p := range core {
		if _, ok := live[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range live {
		if _, ok := union[p]; !ok {
			added = append(added, p)
		}
	}
	// 计数与列表用同一口径（按分支）：原先计数算的是全部路径，新增了 suggest_words 就把
	// 它底下每个子键都算一遍，页面上写着「新增 24」却只列出 12 行。
	missing, added = topLevelFirst(missing), topLevelFirst(added)
	d.MissingCount, d.AddedCount = len(missing), len(added)
	if withLists {
		d.Missing, d.Added = missing[:min(len(missing), maxDiffList)], added[:min(len(added), maxDiffList)]
	}
	return d
}

// topLevelFirst 只保留每条分支的最浅一层：缺了 author 就不必再列 author.name、author.id……
// 结果按字母序。
func topLevelFirst(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if n := len(out); n > 0 {
			last := out[n-1]
			if strings.HasPrefix(p, last+".") || strings.HasPrefix(p, last+"[]") {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}
