package claim

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register 把端点挂到 /me 组下，所以最终路径是 POST /me/anonymous/claim。
// 必须在带 Bearer 中间件的 group 内调用。
func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/anonymous/claim", h.apply)
}

func (h *Handler) apply(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in ClaimInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Apply(c.Request.Context(), id.UserID, &in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func writeErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrPayloadTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	case errors.Is(err, ErrFileNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
