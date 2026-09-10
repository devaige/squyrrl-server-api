package claim

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/quota"

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
	// 配额不足与免费档不同步都走 402，形状与其它端点一致（reason 供客户端分流）。
	// 注意它与 413（payload 过大）语义不同：413 是「这次请求太大」，
	// 402 是「你的账户放不下这些」—— 前者拆小重发即可，后者必须升级或少选。
	if quota.WriteIfQuota(c, err) {
		return
	}
	switch {
	case errors.Is(err, ErrPayloadTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	case errors.Is(err, ErrFileNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
