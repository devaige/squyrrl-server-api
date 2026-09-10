package subscriptions

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc             *Service
	stripeSecret    string
	appleSecret     string
	googlePubsubAud string
}

func NewHandler(svc *Service, stripeSecret, appleSecret, googlePubsubAud string) *Handler {
	return &Handler{
		svc:             svc,
		stripeSecret:    stripeSecret,
		appleSecret:     appleSecret,
		googlePubsubAud: googlePubsubAud,
	}
}

// Register 挂在公网根上 —— webhook 不能走 Bearer 中间件
// 路径：
//
//	POST /webhooks/stripe
//	POST /webhooks/apple
//	POST /webhooks/google
func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/stripe", h.stripe)
	g.POST("/apple", h.apple)
	g.POST("/google", h.google)
}

func (h *Handler) stripe(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}
	if err := VerifyStripeSignature(body, c.GetHeader("Stripe-Signature"), h.stripeSecret, 5*time.Minute); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	evt, err := ParseStripeEvent(body)
	if err != nil {
		// 未处理的事件类型也回 200，让 Stripe 不要重试
		if errors.Is(err, ErrUnknownEvent) {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.Apply(c.Request.Context(), evt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) apple(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}
	if err := VerifyAppleSignature(body, c.GetHeader("X-Squyrrl-Sig"), h.appleSecret); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	evt, err := ParseAppleNotification(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.Apply(c.Request.Context(), evt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) google(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}
	if err := VerifyGoogleOIDC(c.GetHeader("Authorization"), h.googlePubsubAud); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	evt, err := ParseGoogleRTDN(body)
	if err != nil {
		if errors.Is(err, ErrUnknownEvent) {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.Apply(c.Request.Context(), evt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
