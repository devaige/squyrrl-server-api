package parser

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// watchedPaths 从解码用的结构体反射出「适配器代码会读取的字段路径」，写法与结构比对一致：
// 对象键用 . 连接，数组记作 []。
//
// 结构比对只拿这些字段算缺失：上游响应里大量字段随内容可有可无（直播间信息、背景音乐、
// 各语言字幕……），代码不读的字段缺了不影响解析，报出来只是噪音（实测：同一接口两条正常
// 响应差出 20 多个这类字段）。路径直接取自 json 标签，改了解码结构，比对跟着变，不必另外维护。
//
// 停在三类地方：map（键是数据，如推文的 photo / video、小红书的 h264 / h265），
// 自定义反序列化的类型（flexInt、weiboImg 这类按值形态分支的），以及标量。
func watchedPaths(shape any) []string {
	if shape == nil {
		return nil
	}
	set := map[string]struct{}{}
	walkShape(reflect.TypeOf(shape), "", set, 0)
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func walkShape(t reflect.Type, prefix string, out map[string]struct{}, depth int) {
	if depth > 12 {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		walkShape(t.Elem(), prefix+"[]", out, depth+1)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" { // 嵌入的结构体：字段提升到同一层
				walkShape(f.Type, prefix, out, depth)
				continue
			}
			if name == "" {
				name = f.Name
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out[p] = struct{}{}
			walkShape(f.Type, p, out, depth+1)
		}
	}
}

// WatchedPaths 实现 extapi.Catalog：某个接口的适配器读取哪些字段。
func (s AdapterSet) WatchedPaths(slug string) []string {
	a, ok := s[slug]
	if !ok {
		return nil
	}
	return watchedPaths(a.Shape)
}
