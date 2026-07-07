package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// RedditProvider 抓 Reddit 帖子元信息。公开 .json 端点。
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

func (p *RedditProvider) Provider() string    { return "reddit" }
func (p *RedditProvider) SnippetType() string { return "special" }

func (p *RedditProvider) MatchPatterns() []string { return []string{redditIDPattern.String()} }

var _ ManifestProvider = (*RedditProvider)(nil)

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
	req.Header.Set("User-Agent", "Squyrrl/0.1 (+https://squyrrl.app)")

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
					Title       string `json:"title"`
					Subreddit   string `json:"subreddit"`
					Author      string `json:"author"`
					URL         string `json:"url"`
					Permalink   string `json:"permalink"`
					Selftext    string `json:"selftext"`
					Thumbnail   string `json:"thumbnail"`
					Score       int    `json:"score"`
					NumComments int    `json:"num_comments"`
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
	post := raw[0].Data.Children[0].Data

	payload, _ := json.Marshal(map[string]any{
		"url":          uri,
		"post_id":      resourceID,
		"subreddit":    post.Subreddit,
		"external_url": post.URL,
		"permalink":    "https://www.reddit.com" + post.Permalink,
		"thumbnail":    post.Thumbnail,
		"score":        post.Score,
		"num_comments": post.NumComments,
	})
	source, _ := json.Marshal(map[string]any{
		"provider":      "reddit",
		"author_handle": "u/" + post.Author,
		"subreddit":     "r/" + post.Subreddit,
	})

	title := post.Title
	var desc *string
	if post.Selftext != "" {
		rs := []rune(post.Selftext)
		if len(rs) > 280 {
			s := string(rs[:280]) + "…"
			desc = &s
		} else {
			s := post.Selftext
			desc = &s
		}
	}

	return &ParseResult{
		Title:       &title,
		Description: desc,
		Payload:     payload,
		SourceData:  source,
		// post.Thumbnail 常是 "self"/"default"/"nsfw"/"" 等占位串，mediaAsset 只放行真 http 地址。
		Assets: mediaAsset(post.Thumbnail, "thumbnail", ""),
	}, nil
}
