package page

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/quota"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("", h.create)
	g.GET("", h.list)
	g.GET("/:id", h.get)
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
	p, err := h.svc.Create(c.Request.Context(), id.UserID, &in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (h *Handler) list(c *gin.Context) {
	id := auth.MustIdentity(c)
	pages, err := h.svc.List(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if pages == nil {
		pages = []Page{}
	}
	c.JSON(http.StatusOK, gin.H{"items": pages})
}

func (h *Handler) get(c *gin.Context) {
	id := auth.MustIdentity(c)
	pid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	p, err := h.svc.Get(c.Request.Context(), id.UserID, pid)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) update(c *gin.Context) {
	id := auth.MustIdentity(c)
	pid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	var in UpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p, err := h.svc.Update(c.Request.Context(), id.UserID, pid, &in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) delete(c *gin.Context) {
	id := auth.MustIdentity(c)
	pid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id.UserID, pid); err != nil {
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
	case errors.Is(err, ErrSystemReadOnly):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
