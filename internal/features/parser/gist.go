package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// GistProvider 抓 GitHub Gist 元信息（公开 API，无需 token，60 次/小时 anon rate limit）
//
// 支持的 URL 形态：
//   - https://gist.github.com/<user>/<gist_id>   — 现行主流；ID 是任意 alphanumeric
//
// 单段 `gist.github.com/<id>` 形式不在此 provider 范围内：那种 URL
// 既可能是 gist 又可能是 user profile，让 GenericOGProvider 兜底。
type GistProvider struct {
	client *http.Client
}

func NewGistProvider() *GistProvider {
	return &GistProvider{client: &http.Client{Timeout: 8 * time.Second}}
}

var gistIDPattern = regexp.MustCompile(`gist\.github\.com/[^/]+/([a-zA-Z0-9]+)`)

func (p *GistProvider) Provider() string    { return "gist" }
func (p *GistProvider) SnippetType() string { return "special" }

func (p *GistProvider) Match(uri string) (string, bool) {
	m := gistIDPattern.FindStringSubmatch(uri)
	if m == nil {
		return "", false
	}
	return m[1], true
}

type gistFileSummary struct {
	Filename string `json:"filename"`
	Language string `json:"language"`
	Type     string `json:"type"`
	Size     int    `json:"size"`
	RawURL   string `json:"raw_url"`
}

func (p *GistProvider) Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/gists/"+resourceID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.app)")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github gist api %d", resp.StatusCode)
	}

	var raw struct {
		Description string `json:"description"`
		HTMLURL     string `json:"html_url"`
		Owner       struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url"`
			HTMLURL   string `json:"html_url"`
		} `json:"owner"`
		Files map[string]struct {
			Filename string `json:"filename"`
			Language string `json:"language"`
			Type     string `json:"type"`
			Size     int    `json:"size"`
			RawURL   string `json:"raw_url"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	files := make([]gistFileSummary, 0, len(raw.Files))
	for _, f := range raw.Files {
		files = append(files, gistFileSummary{
			Filename: f.Filename,
			Language: f.Language,
			Type:     f.Type,
			Size:     f.Size,
			RawURL:   f.RawURL,
		})
	}

	title := raw.Description
	if title == "" && len(files) > 0 {
		title = files[0].Filename
	}

	payload, _ := json.Marshal(map[string]any{
		"url":      uri,
		"gist_id":  resourceID,
		"html_url": raw.HTMLURL,
		"files":    files,
	})
	source, _ := json.Marshal(map[string]any{
		"provider":          "gist",
		"author_handle":     raw.Owner.Login,
		"author_url":        raw.Owner.HTMLURL,
		"author_avatar_url": raw.Owner.AvatarURL,
	})

	return &ParseResult{
		Title:      &title,
		Payload:    payload,
		SourceData: source,
	}, nil
}
