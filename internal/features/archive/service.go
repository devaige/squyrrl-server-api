package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/snippet"
)

// Service 负责把一个 url 类型 snippet 的原始 HTML 抓下来并作为 element 附上。
//
// 渲染器由 Renderer 接口可插拔提供：
//   - LightRenderer：单次 GET，无 JS，5 MiB 上限（Phase 1 默认）
//   - ChromeRenderer：chromedp headless Chrome，inline 图片+CSS，20 MiB 上限
//
// 切换策略：构造时按优先级数组依次尝试，前面失败回退到后面。
// 这样部署环境没装 Chrome 时自动退化为轻量版，不阻塞功能。
type Service struct {
	pool       *pgxpool.Pool
	snippetSvc *snippet.Service
	fileSvc    *file.Service
	renderers  []Renderer
}

func NewService(pool *pgxpool.Pool, snippetSvc *snippet.Service, fileSvc *file.Service, renderers ...Renderer) *Service {
	if len(renderers) == 0 {
		renderers = []Renderer{NewLightRenderer()}
	}
	return &Service{
		pool:       pool,
		snippetSvc: snippetSvc,
		fileSvc:    fileSvc,
		renderers:  renderers,
	}
}

// Archive 抓取 snippet 引用的 URL 内容并落库为 element
func (s *Service) Archive(ctx context.Context, userID, snippetID uuid.UUID) (*ArchiveResponse, error) {
	snip, err := s.snippetSvc.Get(ctx, userID, snippetID)
	if err != nil {
		return nil, err
	}
	if snip.Type != "url" {
		return nil, ErrNotURLSnippet
	}

	var payload map[string]any
	if err := json.Unmarshal(snip.Payload, &payload); err != nil {
		return nil, err
	}
	urlStr, ok := payload["url"].(string)
	if !ok || strings.TrimSpace(urlStr) == "" {
		return nil, ErrNoURL
	}

	body, mime, rendererName, err := s.render(ctx, urlStr)
	if err != nil {
		return nil, err
	}

	// 服务端代抓的内容不混淆（无客户端密钥）；cipher_hash 与 plain_hash 一致。
	// 客户端下载后 Obfuscator.decrypt 会失败，按 ADR-040 fallback 走原字节展示。
	plain := sha256.Sum256(body)
	uploaded, err := s.fileSvc.Upload(
		ctx,
		plain[:], plain[:],
		int64(len(body)),
		mime,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	altText := "Archived HTML (" + rendererName + ")"
	var elementID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO elements (snippet_id, file_id, order_idx, alt_text)
		VALUES ($1, $2,
			COALESCE((SELECT MAX(order_idx)+1 FROM elements WHERE snippet_id=$1), 0),
			$3)
		RETURNING id`,
		snippetID, uploaded.ID, altText,
	).Scan(&elementID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE snippets SET version = version + 1 WHERE id = $1 AND user_id = $2`,
		snippetID, userID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &ArchiveResponse{
		SnippetID: snippetID,
		FileID:    uploaded.ID,
		ElementID: elementID,
		SizeBytes: int64(len(body)),
		Mime:      mime,
		Renderer:  rendererName,
	}, nil
}

// render 按优先级尝试各 renderer，记录最后一个错误，全部失败则返回。
func (s *Service) render(ctx context.Context, url string) ([]byte, string, string, error) {
	var lastErr error
	for _, r := range s.renderers {
		body, mime, err := r.Render(ctx, url)
		if err == nil {
			return body, mime, r.Name(), nil
		}
		// 体积超限 / 4xx 不算 renderer 故障，不要回退到次级 renderer
		if err == ErrTooLarge {
			return nil, "", "", err
		}
		if _, isHTTP := err.(*httpError); isHTTP {
			return nil, "", "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoRenderer
	}
	return nil, "", "", lastErr
}

type httpError struct {
	Code int
	URL  string
}

func (e *httpError) Error() string {
	return "upstream HTTP " + http.StatusText(e.Code)
}
