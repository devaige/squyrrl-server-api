package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/infra/ratelimit"
)

// 上下文 key：经鉴权中间件后将 Identity 写入 Gin Context
const ctxKeyIdentity = "auth.identity"

type Handler struct {
	svc        *Service
	otpLimiter *ratelimit.Limiter
}

func NewHandler(svc *Service, otpLimiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, otpLimiter: otpLimiter}
}

// RegisterPublic 挂载公开路由（不需要鉴权）
func (h *Handler) RegisterPublic(g *gin.RouterGroup) {
	// 只有这一条挂限流：它是全站唯一「无鉴权 + 直接触发第三方付费调用」的端点。
	g.POST("/email/request", OTPRateLimit(h.otpLimiter), h.requestEmailOTP)
	g.POST("/email/verify", h.verifyEmailOTP)
	g.POST("/refresh", h.refresh)
	g.POST("/qr/redeem", h.redeemQR)
}

// RegisterAuthed 挂载需要鉴权的路由（外部 group 已挂中间件）
func (h *Handler) RegisterAuthed(g *gin.RouterGroup) {
	g.POST("/logout", h.logout)
	g.GET("/me", h.me)
	g.POST("/qr/start", h.startQR)
}

// =============================================================================
// 公开路由
// =============================================================================

func (h *Handler) requestEmailOTP(c *gin.Context) {
	var in RequestEmailOTPInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 不区分"邮箱存在与否"："请求成功"无差别返回
	_ = h.svc.RequestEmailOTP(c.Request.Context(), in.Email)
	c.Status(http.StatusNoContent)
}

func (h *Handler) verifyEmailOTP(c *gin.Context) {
	var in VerifyEmailOTPInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.VerifyEmailOTP(c.Request.Context(), in.Email, in.Code, in.Device.Name, in.Device.Platform)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "邮箱或验证码错误"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) refresh(c *gin.Context) {
	var in RefreshInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pair, err := h.svc.Refresh(c.Request.Context(), in.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrSessionRevoked) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "refresh_token 无效或已过期"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, pair)
}

// =============================================================================
// 鉴权后路由
// =============================================================================

func (h *Handler) logout(c *gin.Context) {
	id := MustIdentity(c)
	if err := h.svc.Logout(c.Request.Context(), id.SessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// =============================================================================
// QR 扫码绑定
// =============================================================================

func (h *Handler) startQR(c *gin.Context) {
	id := MustIdentity(c)
	qr, err := h.svc.IssueQRCode(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, qr)
}

func (h *Handler) redeemQR(c *gin.Context) {
	var in RedeemQRInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.RedeemQRCode(c.Request.Context(), in.Code, in.Device.Name, in.Device.Platform)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "绑定码无效或已过期"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) me(c *gin.Context) {
	id := MustIdentity(c)
	user, err := h.svc.GetUser(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user":      user,
		"device_id": id.DeviceID,
	})
}

// =============================================================================
// 上下文工具
// =============================================================================

// MustIdentity 在挂了 Middleware 的路由内取 Identity，缺失时 panic 是合约违反
func MustIdentity(c *gin.Context) *Identity {
	return c.MustGet(ctxKeyIdentity).(*Identity)
}

func setIdentity(c *gin.Context, id *Identity) {
	c.Set(ctxKeyIdentity, id)
}
