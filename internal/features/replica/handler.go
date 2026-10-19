package replica

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/quota"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/pull", h.pull)
	g.GET("/manifest", h.manifest)
	g.POST("/push", h.push)
}

// GET /sync/pull?since=<seq>&limit=<n>
func (h *Handler) pull(c *gin.Context) {
	id := auth.MustIdentity(c)
	since, err := intQuery(c, "since")
	if err != nil || since < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 since"})
		return
	}
	limit, err := intQuery(c, "limit")
	if err != nil || limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 limit"})
		return
	}
	out, err := h.svc.Pull(c.Request.Context(), id.UserID, since, int(limit))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

// GET /sync/manifest
func (h *Handler) manifest(c *gin.Context) {
	id := auth.MustIdentity(c)
	out, err := h.svc.Manifest(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

// POST /sync/push
func (h *Handler) push(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in PushInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	results, err := h.svc.Push(c.Request.Context(), id.UserID, in.Mutations)
	if err != nil {
		if quota.WriteIfQuota(c, err) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, PushOutput{Results: results})
}

func intQuery(c *gin.Context, key string) (int64, error) {
	v := c.Query(key)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}
