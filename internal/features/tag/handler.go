package tag

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/quota"
)

// tag 包没有 service 层（repo 足够薄），门槛因此挂在 handler 上。
// 其它 feature 一律在 service 层设卡 —— 那里能覆盖所有调用方。
type Handler struct {
	repo  *Repo
	quota *quota.Service
}

func NewHandler(repo *Repo, q *quota.Service) *Handler {
	return &Handler{repo: repo, quota: q}
}

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("", h.create)
	g.GET("", h.list)
	g.PATCH("/:id", h.update)
	g.DELETE("/:id", h.delete)
}

func (h *Handler) create(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in CreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.quota.CheckTagCreate(c.Request.Context(), id.UserID); err != nil {
		writeErr(c, err)
		return
	}
	t, err := h.repo.Create(c.Request.Context(), id.UserID, &in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, t)
}

func (h *Handler) list(c *gin.Context) {
	id := auth.MustIdentity(c)
	tags, err := h.repo.List(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if tags == nil {
		tags = []Tag{}
	}
	c.JSON(http.StatusOK, gin.H{"items": tags})
}

func (h *Handler) update(c *gin.Context) {
	id := auth.MustIdentity(c)
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	var in UpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	t, err := h.repo.Rename(c.Request.Context(), id.UserID, tid, in.Name)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

func (h *Handler) delete(c *gin.Context) {
	id := auth.MustIdentity(c)
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	if err := h.repo.Delete(c.Request.Context(), id.UserID, tid); err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func writeErr(c *gin.Context, err error) {
	// 配额与档位门槛统一走 402，响应体形状由 quota 包持有 —— 散落成多份必然漂移。
	if quota.WriteIfQuota(c, err) {
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrNameDuplicate):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
