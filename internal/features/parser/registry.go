package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Registry 是所有 Parser 的索引表。后续添加新 provider 只需 Register 一次，无需改动核心流程。
type Registry struct {
	parsers []Parser
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Register(p Parser) {
	r.parsers = append(r.parsers, p)
}

// Find 顺序遍历已注册的 Parser；首个 Match 命中者即作为该 URI 的解析者
func (r *Registry) Find(uri string) (Parser, string, bool) {
	for _, p := range r.parsers {
		if id, ok := p.Match(uri); ok {
			return p, id, true
		}
	}
	return nil, "", false
}

// Providers 仅用于诊断/日志
func (r *Registry) Providers() []string {
	out := make([]string, len(r.parsers))
	for i, p := range r.parsers {
		out[i] = p.Provider()
	}
	return out
}

// Manifest 汇总所有实现了 ManifestProvider 的 parser 的匹配规则，供客户端本地判断。
// 顺序沿用注册顺序（server.go 里固定），保证 Version 哈希稳定可复现。
func (r *Registry) Manifest() Manifest {
	entries := make([]ManifestEntry, 0, len(r.parsers))
	for _, p := range r.parsers {
		mp, ok := p.(ManifestProvider) // 通用 OG 兜底不实现此接口，天然被排除
		if !ok {
			continue
		}
		patterns := mp.MatchPatterns()
		if len(patterns) == 0 {
			continue
		}
		entries = append(entries, ManifestEntry{ID: p.Provider(), Patterns: patterns})
	}
	return Manifest{Version: manifestVersion(entries), Providers: entries}
}

// manifestVersion 用内容哈希当版本号——只随 entries 内容变化，客户端据此比对是否需换缓存。
// 取前 6 字节（12 hex）足够区分，短且省流量。
func manifestVersion(entries []ManifestEntry) string {
	b, _ := json.Marshal(entries)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}
