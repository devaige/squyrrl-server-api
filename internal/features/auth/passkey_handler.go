package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// PasskeyHandler 注册 4 个端点：
//
// 公开（不要求 Bearer）：
//   POST /auth/passkey/login/begin
//   POST /auth/passkey/login/finish
//
// 鉴权（要求 Bearer）：
//   POST /auth/passkey/register/begin
//   POST /auth/passkey/register/finish
type PasskeyHandler struct {
	svc *PasskeyService
}

func NewPasskeyHandler(svc *PasskeyService) *PasskeyHandler { return &PasskeyHandler{svc: svc} }

func (h *PasskeyHandler) RegisterPublic(g *gin.RouterGroup) {
	g.POST("/login/begin", h.loginBegin)
	g.POST("/login/finish", h.loginFinish)
}

func (h *PasskeyHandler) RegisterAuthed(g *gin.RouterGroup) {
	g.POST("/register/begin", h.registerBegin)
	g.POST("/register/finish", h.registerFinish)
}

// =============================================================================
// 注册（已登录用户绑定新 passkey）
// =============================================================================

func (h *PasskeyHandler) registerBegin(c *gin.Context) {
	id := MustIdentity(c)
	options, sessID, err := h.svc.BeginRegistration(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"publicKey": options, "session_id": sessID})
}

func (h *PasskeyHandler) registerFinish(c *gin.Context) {
	id := MustIdentity(c)
	var in struct {
		SessionID string          `json:"session_id" binding:"required"`
		Response  json.RawMessage `json:"response" binding:"required"`
		Name      string          `json:"name"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.FinishRegistration(c.Request.Context(), id.UserID, in.SessionID, in.Response, in.Name); err != nil {
		writePasskeyErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// =============================================================================
// 登录（无密码登录）
// =============================================================================

func (h *PasskeyHandler) loginBegin(c *gin.Context) {
	var in struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	options, sessID, err := h.svc.BeginLogin(c.Request.Context(), in.Email)
	if err != nil {
		writePasskeyErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"publicKey": options, "session_id": sessID})
}

func (h *PasskeyHandler) loginFinish(c *gin.Context) {
	var in struct {
		SessionID string          `json:"session_id" binding:"required"`
		Email     string          `json:"email"      binding:"required,email"`
		Response  json.RawMessage `json:"response"   binding:"required"`
		Device    DeviceInput     `json:"device"     binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.FinishLogin(c.Request.Context(), in.SessionID, in.Email, in.Response, in.Device.Name, in.Device.Platform)
	if err != nil {
		writePasskeyErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func writePasskeyErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "凭据无效或会话已过期"})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "凭据无效或会话已过期"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}
