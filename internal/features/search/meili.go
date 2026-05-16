package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Meili 是对 Meilisearch HTTP API 的极简封装。
// 不引入 meilisearch-go 是为了：(a) 控制依赖；(b) 我们只用到 4 个端点（add、delete、search、index settings）。
// 失败行为：所有网络调用错误 swallow 在 caller 侧（变成 slog warn），永远不阻断 snippet CRUD。
type Meili struct {
	baseURL string
	apiKey  string
	index   string
	client  *http.Client
}

// Enabled 当 baseURL 为空时整个 Meili 实例为 nil-safe noop。
// caller 通过 m == nil 或 !m.Enabled() 来分流到 ILIKE fallback。
func (m *Meili) Enabled() bool { return m != nil && m.baseURL != "" }

func NewMeili(baseURL, apiKey, index string) *Meili {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &Meili{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		index:   index,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Document 是写入 Meili 的扁平结构。id 必须是字符串（uuid 形式 ok）。
// user_id 作为过滤字段，防止跨用户搜出别人的内容。
type Document struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	PageID      string    `json:"page_id,omitempty"`
	TagIDs      []string  `json:"tag_ids,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (m *Meili) Upsert(ctx context.Context, docs []Document) error {
	if !m.Enabled() || len(docs) == 0 {
		return nil
	}
	body, _ := json.Marshal(docs)
	_, err := m.do(ctx, http.MethodPost, "/indexes/"+m.index+"/documents", body)
	return err
}

func (m *Meili) Delete(ctx context.Context, ids ...uuid.UUID) error {
	if !m.Enabled() || len(ids) == 0 {
		return nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	body, _ := json.Marshal(strs)
	_, err := m.do(ctx, http.MethodPost, "/indexes/"+m.index+"/documents/delete-batch", body)
	return err
}

type Hit struct {
	ID string `json:"id"`
}

type searchResponse struct {
	Hits []Hit `json:"hits"`
}

// SearchIDs 返回命中的 snippet id 列表（按相关性排序）。
// 限定 user_id 过滤；q 为空时返回 0 命中（让 caller 走 plain list）。
func (m *Meili) SearchIDs(ctx context.Context, userID uuid.UUID, q string, limit int) ([]uuid.UUID, error) {
	if !m.Enabled() || strings.TrimSpace(q) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	req := map[string]any{
		"q":           q,
		"filter":      fmt.Sprintf(`user_id = "%s"`, userID.String()),
		"limit":       limit,
		"attributesToRetrieve": []string{"id"},
	}
	body, _ := json.Marshal(req)
	resp, err := m.do(ctx, http.MethodPost, "/indexes/"+m.index+"/search", body)
	if err != nil {
		return nil, err
	}
	var out searchResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(out.Hits))
	for _, h := range out.Hits {
		if u, err := uuid.Parse(h.ID); err == nil {
			ids = append(ids, u)
		}
	}
	return ids, nil
}

// EnsureIndex 是启动时一次性配置：声明 filterable / searchable / sortable 属性。
// 若 index 不存在 Meili 会自动创建，本端点幂等。
func (m *Meili) EnsureIndex(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}
	// primary key
	createBody, _ := json.Marshal(map[string]string{"uid": m.index, "primaryKey": "id"})
	if _, err := m.do(ctx, http.MethodPost, "/indexes", createBody); err != nil {
		// 已存在（4xx）忽略
		var herr *httpError
		if !errors.As(err, &herr) || herr.code < 400 || herr.code >= 500 {
			return err
		}
	}
	settings := map[string]any{
		"searchableAttributes": []string{"title", "description"},
		"filterableAttributes": []string{"user_id", "type", "page_id", "tag_ids"},
		"sortableAttributes":   []string{"updated_at"},
	}
	body, _ := json.Marshal(settings)
	_, err := m.do(ctx, http.MethodPatch, "/indexes/"+m.index+"/settings", body)
	return err
}

func (m *Meili) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &httpError{code: resp.StatusCode, body: string(data)}
	}
	return data, nil
}

type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string { return fmt.Sprintf("meili http %d: %s", e.code, e.body) }
