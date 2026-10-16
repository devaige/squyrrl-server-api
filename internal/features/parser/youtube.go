package parser

import (
	"regexp"
)

// YouTubeProvider 是 YouTube 平台：只负责认链接。
//
// 它**没有内置免费来源**。oEmbed 只有标题、作者和缩略图，属于客户端的免费「暂存/补全」，
// 不算解析（ADR-080）—— 所以 YouTube 能不能解析，取决于后台有没有启用付费上游接口。
//
// 命中链接形式：youtube.com/watch?v=ID、youtu.be/ID、youtube.com/shorts/ID、youtube.com/embed/ID
type YouTubeProvider struct{}

func NewYouTubeProvider() *YouTubeProvider { return &YouTubeProvider{} }

var ytIDPattern = regexp.MustCompile(
	`(?:youtube\.com/(?:watch\?v=|embed/|shorts/)|youtu\.be/)([A-Za-z0-9_-]{11})`,
)

func (p *YouTubeProvider) Provider() string    { return "youtube" }
func (p *YouTubeProvider) SnippetType() string { return "special" }

// MatchPatterns 导出与 Match 完全同一份正则（.String() 取源串），进 manifest 给客户端本地判断。
func (p *YouTubeProvider) MatchPatterns() []string { return []string{ytIDPattern.String()} }

var _ ManifestProvider = (*YouTubeProvider)(nil)

func (p *YouTubeProvider) Match(uri string) (string, bool) {
	m := ytIDPattern.FindStringSubmatch(uri)
	if m == nil {
		return "", false
	}
	return m[1], true
}
