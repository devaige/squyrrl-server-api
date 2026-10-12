package users

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterInternal 挂在 /internal/admin 下，受 X-Internal-Token 头保护，供管理后台调用。
func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.GET("/users", h.search)
}

func (h *Handler) search(c *gin.Context) {
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		limit = n
	}
	items, err := h.svc.Search(c.Request.Context(), strings.TrimSpace(c.Query("q")), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
