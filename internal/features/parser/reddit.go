package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// RedditProvider 是 Reddit 的内置免费来源：公开 .json 端点。它排在付费接口前面，
// 但公开 JSON 对机房 IP 常年 403（2026-10-16 实测，本机同样 403），多数时候要靠付费接口兜底。
// 两个来源产出同一种 post 形状（ADR-080）。
//
// 支持：
//   - reddit.com/r/<sub>/comments/<id>/...
//   - reddit.com/comments/<id>...
//   - redd.it/<id>
type RedditProvider struct {
	client *http.Client
}

func NewRedditProvider() *RedditProvider {
	return &RedditProvider{client: &http.Client{Timeout: 8 * time.Second}}
}

var redditIDPattern = regexp.MustCompile(
	`(?:reddit\.com/r/[^/]+/comments/([a-z0-9]+)|reddit\.com/comments/([a-z0-9]+)|redd\.it/([a-z0-9]+))`,
)

func (p *RedditProvider) Provider() string    { return PlatformReddit }
func (p *RedditProvider) SnippetType() string { return "special" }

func (p *RedditProvider) MatchPatterns() []string { return []string{redditIDPattern.String()} }

var (
	_ ManifestProvider = (*RedditProvider)(nil)
	_ BuiltinParser    = (*RedditProvider)(nil)
)

func (p *RedditProvider) Match(uri string) (string, bool) {
	m := redditIDPattern.FindStringSubmatch(uri)
	if m == nil {
		return "", false
	}
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return m[i], true
		}
	}
	return "", false
}

func (p *RedditProvider) Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.reddit.com/comments/"+resourceID+".json", nil)
	if err != nil {
		return nil, err
	}
	// reddit 强制要求自定义 UA，否则常常 429
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.com)")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reddit api %d", resp.StatusCode)
	}

	// reddit json 形如 [<post listing>, <comment listing>]
	var raw []struct {
		Data struct {
			Children []struct {
				Data struct {
					Title       string  `json:"title"`
					Author      string  `json:"author"`
					URL         string  `json:"url"`
					Domain      string  `json:"domain"`
					Permalink   string  `json:"permalink"`
					Selftext    string  `json:"selftext"`
					Thumbnail   string  `json:"thumbnail"`
					CreatedUTC  float64 `json:"created_utc"`
					Score       int64   `json:"score"`
					NumComments int64   `json:"num_comments"`
				} `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw[0].Data.Children) == 0 {
		return nil, fmt.Errorf("reddit post not found")
	}
	r := raw[0].Data.Children[0].Data

	p0 := &post{
		ID:        resourceID,
		URL:       "https://www.reddit.com" + r.Permalink,
		Title:     r.Title,
		Text:      r.Selftext,
		CreatedAt: unixRFC3339(int64(r.CreatedUTC)),
		Stats:     &postStats{Likes: i64(r.Score), Comments: i64(r.NumComments)},
		Cover:     r.Thumbnail,
	}
	if r.URL != "" && !redditHosted(r.Domain) {
		p0.Text = strings.TrimSpace(p0.Text + "\n\n" + r.URL) // 链接帖的外链，同付费接口
	}
	if r.Author != "" && r.Author != "[deleted]" {
		p0.Author = &postAuthor{Name: r.Author, Handle: "u/" + r.Author, URL: "https://www.reddit.com/user/" + r.Author}
	}
	if !strings.HasPrefix(p0.Cover, "http") { // thumbnail 常是 "self"/"default"/"nsfw" 这类占位串
		p0.Cover = ""
	}
	return p0.result(PlatformReddit), nil
}
