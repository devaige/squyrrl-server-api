package snippet

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
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
	s, err := h.svc.Create(c.Request.Context(), id.UserID, &in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Header("ETag", `"`+strconv.FormatInt(s.Version, 10)+`"`)
	c.JSON(http.StatusCreated, s)
}

func (h *Handler) list(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in ListInput
	if err := c.ShouldBindQuery(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if v := c.Query("page_id"); v != "" {
		pid, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 page_id"})
			return
		}
		in.PageID = &pid
	}
	if v := c.Query("tag_id"); v != "" {
		tid, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 tag_id"})
			return
		}
		in.TagID = &tid
	}
	// 全文搜索：q 由 repo 层做 PostgreSQL ILIKE（title / description 子串匹配）。
	out, err := h.svc.List(c.Request.Context(), id.UserID, &in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) get(c *gin.Context) {
	id := auth.MustIdentity(c)
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	s, err := h.svc.Get(c.Request.Context(), id.UserID, sid)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Header("ETag", `"`+strconv.FormatInt(s.Version, 10)+`"`)
	c.JSON(http.StatusOK, s)
}

func (h *Handler) update(c *gin.Context) {
	id := auth.MustIdentity(c)
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}

	var in UpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 若 If-Match 头存在则覆盖 body 中的 version；body version 仍是兜底
	if v, ok := parseIfMatch(c.GetHeader("If-Match")); ok {
		in.Version = v
	}
	if in.Version == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 version（请求体或 If-Match 头）"})
		return
	}

	s, err := h.svc.Update(c.Request.Context(), id.UserID, sid, &in)
	if err != nil {
		var ce *ConflictError
		if errors.As(err, &ce) {
			c.JSON(http.StatusConflict, gin.H{
				"error":            "version conflict",
				"current_version":  ce.CurrentVersion,
				"loser_snippet_id": ce.LoserSnippetID,
				"loser_page_id":    ce.LoserPageID,
			})
			return
		}
		writeErr(c, err)
		return
	}
	c.Header("ETag", `"`+strconv.FormatInt(s.Version, 10)+`"`)
	c.JSON(http.StatusOK, s)
}

func (h *Handler) delete(c *gin.Context) {
	id := auth.MustIdentity(c)
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}

	v, ok := parseIfMatch(c.GetHeader("If-Match"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 If-Match 头"})
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id.UserID, sid, v); err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func parseIfMatch(h string) (int64, bool) {
	h = strings.TrimSpace(h)
	h = strings.Trim(h, `"`) // 接受 "5" 或 5 两种写法
	if h == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(h, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func writeErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrVersionConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, ErrTagNotOwned):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrPageNotOwned):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrFileNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
