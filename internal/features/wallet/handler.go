package wallet

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

// RegisterUser 用户端 — 受 Bearer 中间件保护
func (h *Handler) RegisterUser(g *gin.RouterGroup) {
	g.GET("/wallet", h.getWallet)
}

// RegisterInternal 内部端 — 受 X-Internal-Token 保护，供管理后台 / 支付回调使用
func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.POST("/credits/grant", h.grantCredits)
}

func (h *Handler) getWallet(c *gin.Context) {
	id := auth.MustIdentity(c)
	w, err := h.svc.GetWallet(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, w)
}

func (h *Handler) grantCredits(c *gin.Context) {
	var in GrantInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Grant(c.Request.Context(), in.UserID, in.Delta, in.Reason)
	if err != nil {
		// 扣得太多或数值越界都是请求本身的问题，回 400 并把当前余额带在消息里，
		// 后台才知道该改填多少；回 500 会让运营以为是服务挂了而反复重试。
		if errors.Is(err, ErrNegativeBalance) || errors.Is(err, ErrGrantOverflow) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}
