package snippet

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 后台查看某个用户的碎片（2026-10-14，用户决策）。只读，只挂在 /internal/admin 下。
//
// text_content 原样回混淆字节：api 不持有混淆密钥（ADR-003），解混淆由后台服务端
// 自己做，不为了后台把密钥搬进 api。附件字节同理，后台凭 /internal/admin/files/:id/ticket
// 去边缘取。

const (
	AdminDefaultLimit = 50
	AdminMaxLimit     = 100
)

type AdminListInput struct {
	// 含回收站：后台排查「我删掉的东西去哪了」时正需要它们。
	IncludeDeleted bool `form:"include_deleted"`
	Limit          int  `form:"limit"`
	Offset         int  `form:"offset"`
}

// AdminListOutput 把页面、标签名一并带回，后台不必再为每个 ID 单独查一次。
// 两张表按用户全取（含软删的页面）：量级是几十行，而碎片可能挂在已删的页面上。
type AdminListOutput struct {
	Items []Snippet            `json:"items"`
	Total int                  `json:"total"`
	Pages map[uuid.UUID]string `json:"pages"`
	Tags  map[uuid.UUID]string `json:"tags"`
}

func clampAdminLimit(n int) int {
	if n <= 0 {
		return AdminDefaultLimit
	}
	return min(n, AdminMaxLimit)
}

type AdminRepo struct {
	pool *pgxpool.Pool
}

func NewAdminRepo(pool *pgxpool.Pool) *AdminRepo { return &AdminRepo{pool: pool} }

// List 按 updated_at 倒序分页。用 offset 而不是用户侧的 since 游标：后台要的是
// 「翻到第几页」，不是增量同步。
func (r *AdminRepo) List(ctx context.Context, userID uuid.UUID, in AdminListInput) (*AdminListOutput, error) {
	cond := "s.user_id = $1"
	if !in.IncludeDeleted {
		cond += " AND s.deleted_at IS NULL"
	}
	out := &AdminListOutput{
		Items: []Snippet{},
		Pages: map[uuid.UUID]string{},
		Tags:  map[uuid.UUID]string{},
	}
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM snippets s WHERE `+cond, userID).
		Scan(&out.Total); err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+snippetCols+`
		FROM snippets s
		WHERE `+cond+`
		ORDER BY s.updated_at DESC, s.id ASC
		LIMIT $2 OFFSET $3`,
		userID, clampAdminLimit(in.Limit), max(in.Offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s, err := scanSnippet(rows)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := r.names(ctx, `SELECT id, name FROM pages WHERE user_id = $1`, userID, out.Pages); err != nil {
		return nil, err
	}
	if err := r.names(ctx, `SELECT id, name FROM tags WHERE user_id = $1 AND deleted_at IS NULL`, userID, out.Tags); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *AdminRepo) names(ctx context.Context, q string, userID uuid.UUID, into map[uuid.UUID]string) error {
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		into[id] = name
	}
	return rows.Err()
}

// AdminHandler 是后台查看碎片的内部端。
type AdminHandler struct {
	repo *AdminRepo
}

func NewAdminHandler(repo *AdminRepo) *AdminHandler { return &AdminHandler{repo: repo} }

// RegisterInternal 挂在 /internal/admin 下，受 X-Internal-Token 头保护，供管理后台调用。
//
//	GET /users/:id/snippets?include_deleted=true&limit=50&offset=0
func (h *AdminHandler) RegisterInternal(g *gin.RouterGroup) {
	g.GET("/users/:id/snippets", h.list)
}

func (h *AdminHandler) list(c *gin.Context) {
	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var in AdminListInput
	if err := c.ShouldBindQuery(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	out, err := h.repo.List(c.Request.Context(), userID, in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}
