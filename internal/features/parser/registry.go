package parser

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
