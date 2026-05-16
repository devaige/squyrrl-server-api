package tg

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

// =============================================================================
// 用户端：受 Bearer 中间件保护
// =============================================================================
func (h *Handler) RegisterUser(g *gin.RouterGroup) {
	g.POST("/binding/start", h.startBinding)
}

func (h *Handler) startBinding(c *gin.Context) {
	id := auth.MustIdentity(c)
	code, err := h.svc.IssueCode(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, code)
}

// =============================================================================
// Bot 内部端：受 X-Internal-Token 头保护
// =============================================================================
func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.POST("/binding/complete", h.completeBinding)
	g.POST("/snippet", h.forwardSnippet)
}

func (h *Handler) completeBinding(c *gin.Context) {
	var in CompleteBindingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID, err := h.svc.CompleteBinding(c.Request.Context(), in.Code, in.TGUserID)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeInvalid):
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		case errors.Is(err, ErrAlreadyBound):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, CompleteBindingResponse{UserID: userID})
}

func (h *Handler) forwardSnippet(c *gin.Context) {
	var in SnippetForwardInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.CreateForwardedSnippet(c.Request.Context(), in.TGUserID, &in.Snippet)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotBound):
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusCreated, res)
}

// =============================================================================
// 内部 token 中间件
// =============================================================================
func InternalAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" || c.GetHeader("X-Internal-Token") != token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "internal token required"})
			return
		}
		c.Next()
	}
}
