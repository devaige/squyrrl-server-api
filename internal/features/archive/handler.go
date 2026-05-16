package archive

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/snippet"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/snippets/:id/archive", h.archive)
}

func (h *Handler) archive(c *gin.Context) {
	id := auth.MustIdentity(c)
	snippetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 snippet id"})
		return
	}
	res, err := h.svc.Archive(c.Request.Context(), id.UserID, snippetID)
	if err != nil {
		switch {
		case errors.Is(err, snippet.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "snippet 不存在"})
		case errors.Is(err, ErrNotURLSnippet):
			c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持对 URL 碎片归档"})
		case errors.Is(err, ErrNoURL):
			c.JSON(http.StatusBadRequest, gin.H{"error": "snippet payload 缺少 url 字段"})
		case errors.Is(err, ErrTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "目标内容超过 5MiB"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
