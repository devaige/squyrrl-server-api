package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// YouTube oEmbed 提供器。public、免 key、稳定。
//   - oEmbed 端点：https://www.youtube.com/oembed?url=<URL>&format=json
//   - 命中链接形式：youtube.com/watch?v=ID、youtu.be/ID、youtube.com/shorts/ID、youtube.com/embed/ID
//
// Phase 2 可加 yt-dlp 抓更多元数据（时长、字幕、画质列表等）。
type YouTubeProvider struct {
	client *http.Client
}

func NewYouTubeProvider() *YouTubeProvider {
	return &YouTubeProvider{
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

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

func (p *YouTubeProvider) Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error) {
	endpoint := "https://www.youtube.com/oembed?url=" + url.QueryEscape(uri) + "&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.com)")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube oembed status %d", resp.StatusCode)
	}

	var raw struct {
		Title           string `json:"title"`
		AuthorName      string `json:"author_name"`
		AuthorURL       string `json:"author_url"`
		ProviderName    string `json:"provider_name"`
		ThumbnailURL    string `json:"thumbnail_url"`
		ThumbnailWidth  int    `json:"thumbnail_width"`
		ThumbnailHeight int    `json:"thumbnail_height"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	payload, _ := json.Marshal(map[string]any{
		"url":              uri,
		"video_id":         resourceID,
		"thumbnail_url":    raw.ThumbnailURL,
		"thumbnail_width":  raw.ThumbnailWidth,
		"thumbnail_height": raw.ThumbnailHeight,
	})
	source, _ := json.Marshal(map[string]any{
		"provider":      "youtube",
		"author_handle": raw.AuthorName,
		"author_url":    raw.AuthorURL,
	})

	title := raw.Title
	return &ParseResult{
		Title:      &title,
		Payload:    payload,
		SourceData: source,
		// oEmbed 缩略图恒为 jpg；提出来让客户端下载→挂 element，坍塌成普通碎片后仍留存。
		Assets: mediaAsset(raw.ThumbnailURL, "thumbnail", "image/jpeg"),
	}, nil
}
