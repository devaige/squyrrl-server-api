package extapi

import (
	"encoding/json"
	"sort"
	"strings"
)

// 结构比对：上游返回的 data 没有契约，会随内容形态（图文 / 视频 / 动图……）和上游改版变化。
// 「示例」是我们核对过的响应，每种形态一份，在代码里维护（Catalog.Examples，用户决定 2026-10-19）。
// 线上采样拿来和示例逐字段比，只比字段有没有、不比值：
//
//	缺失 = 和最接近的那份示例比，适配器代码会读、示例里有、这一份却没有的字段 ——
//	       多半是上游改了结构，或者是一种还没收录的新形态；
//	新增 = 所有示例都没有、这一份有的字段 —— 只作提示，可能是新形态。
//
// 缺失按最接近的示例算：结构本来就随内容形态变（视频推文有 video、图片推文没有），对得上
// 某一种已知形态就不算缺。原先按「所有示例共有的字段」算，每收录一种形态共有字段就少一些，
// 视频推文丢了视频字段也不再报 —— 示例越多比对越松（改于 2026-10-19）。
//
// 只看「代码会读」的字段：大量字段随内容可有可无（直播间、背景音乐、各语言字幕），实测同一接口
// 两条正常响应差出 20 多个这类字段，而代码不读的字段缺了不影响解析。

const (
	maxShapeDepth = 12
	maxShapePaths = 20000
	maxDiffList   = 100
)

// StructureDiff 是一份响应相对于示例的结构差异。
type StructureDiff struct {
	Baseline     int      `json:"baseline"`          // 参与比对的示例数；0 表示还没有示例，无从比较
	Closest      string   `json:"closest,omitempty"` // 最接近的那份示例（它的备注），缺失是相对它算的
	MissingCount int      `json:"missing_count"`     // 按分支计：缺了 author 只算一处，不再数 author 底下的子键
	AddedCount   int      `json:"added_count"`
	Missing      []string `json:"missing,omitempty"` // 最多 maxDiffList 条，计数不受这个截断影响
	Added        []string `json:"added,omitempty"`
}

// ExampleDiff 是一份示例相对同接口另一份示例的结构差异。线上采样对上某份示例就不再报差异，
// 收录进来的形态差在哪由这里看（用户决定 2026-10-19）。比全部字段，不只代码会读的：要看的是
// 两种形态本身差在哪。
type ExampleDiff struct {
	Other        string   `json:"other"`         // 另一份示例（它的备注）
	AddedCount   int      `json:"added_count"`   // 这一份有、另一份没有；按分支计
	MissingCount int      `json:"missing_count"` // 另一份有、这一份没有
	Added        []string `json:"added,omitempty"`
	Missing      []string `json:"missing,omitempty"`
}

// diffable 判断一条线上采样要不要和示例比：只比上游正常应答（2xx）的那些。
// 上游明说失败的应答（TikHub 的 400「本次请求不会被扣费」）是一段错误说明，拿去比会报出
// 一长串「缺失」，看起来像上游改了结构，其实只是这次没取到。
func diffable(x Sample) bool {
	return x.HTTPStatus >= 200 && x.HTTPStatus < 300
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

// baseline 是一份示例算好的字段集合。示例单份可达几百 KB，一次比多条采样时只解析一遍。
type baseline struct {
	of    int // 在 examples 里的下标
	note  string
	shape map[string]struct{}
}

// baselinesOf 把示例解析成比对用的字段集合；解析不了的跳过（Baseline 计数也不算它）。
func baselinesOf(examples []Example) []baseline {
	out := make([]baseline, 0, len(examples))
	for i, ex := range examples {
		var v any
		if json.Unmarshal(ex.Data, &v) != nil {
			continue
		}
		out = append(out, baseline{of: i, note: ex.Note, shape: shapeOf(v)})
	}
	return out
}

// compareExamples 把同一接口的示例两两比对，结果填进各自的 Versus。
func compareExamples(examples []Example, bases []baseline) {
	for _, a := range bases {
		for _, b := range bases {
			if a.of == b.of {
				continue
			}
			added, missing := topLevelFirst(pathsNotIn(a.shape, b.shape)), topLevelFirst(pathsNotIn(b.shape, a.shape))
			examples[a.of].Versus = append(examples[a.of].Versus, ExampleDiff{
				Other:        b.note,
				AddedCount:   len(added),
				MissingCount: len(missing),
				Added:        added[:min(len(added), maxDiffList)],
				Missing:      missing[:min(len(missing), maxDiffList)],
			})
		}
	}
}

func pathsNotIn(a, b map[string]struct{}) []string {
	var out []string
	for p := range a {
		if _, ok := b[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// diffAgainst 把一份响应和一组示例比。watched 为空时缺失按全部字段算。
//
// 最接近 = 缺失（按分支）最少；一样少时取这一份比它多出的字段最少的 —— 图片推文和视频推文
// 都不缺字段时，带着 video 的这一份更像视频推文。
func diffAgainst(response json.RawMessage, bases []baseline, watched []string, withLists bool) *StructureDiff {
	d := &StructureDiff{Baseline: len(bases)}
	if len(bases) == 0 {
		return d
	}
	live := shapeOf(payloadOf(response))

	var keep map[string]struct{}
	if len(watched) > 0 {
		keep = make(map[string]struct{}, len(watched))
		for _, p := range watched {
			keep[p] = struct{}{}
		}
	}

	var missing []string
	bestExtra := 0
	for i, b := range bases {
		var miss []string
		for p := range b.shape {
			if _, read := keep[p]; keep != nil && !read {
				continue
			}
			if _, ok := live[p]; !ok {
				miss = append(miss, p)
			}
		}
		miss = topLevelFirst(miss)
		extra := 0
		for p := range live {
			if _, ok := b.shape[p]; !ok {
				extra++
			}
		}
		if i == 0 || len(miss) < len(missing) || (len(miss) == len(missing) && extra < bestExtra) {
			missing, bestExtra, d.Closest = miss, extra, b.note
		}
	}

	var added []string
	for p := range live {
		known := false
		for _, b := range bases {
			if _, ok := b.shape[p]; ok {
				known = true
				break
			}
		}
		if !known {
			added = append(added, p)
		}
	}
	// 计数与列表用同一口径（按分支）：原先计数算的是全部路径，新增了 suggest_words 就把
	// 它底下每个子键都算一遍，页面上写着「新增 24」却只列出 12 行。
	added = topLevelFirst(added)
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
