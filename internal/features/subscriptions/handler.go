package subscriptions

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/pricing"
)

type Handler struct {
	svc             *Service
	checkoutSvc     *Checkout
	stripeSecret    string
	appleSecret     string
	googlePubsubAud string
}

// RegisterUser 挂用户端购买入口（受 Bearer 保护）。
func (h *Handler) RegisterUser(g *gin.RouterGroup) {
	g.POST("/checkout", h.checkout)
}

// checkout 发起一次购买。
//
// 只接受 SKU（kind/tier/period），**金额与数量一概由服务端从自己的价目表推导**。
// 让客户端提交价格或容量，等于让它自己决定买多少 —— 而 webhook 完全信任
// metadata，那是唯一能说明「买了什么」的地方。
func (h *Handler) checkout(c *gin.Context) {
	if h.checkoutSvc == nil || !h.checkoutSvc.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": ErrCheckoutUnconfigured.Error()})
		return
	}
	var in CheckoutRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 先按自己的目录核对 SKU 再去问 Stripe：价目表里多一个我们没上架的键，
	// 不应该成为一条能买到未定义商品的通路。
	var storageGB int
	var credits int64
	switch in.Kind {
	case "plan":
		if !entitlement.Known(in.Tier) || in.Tier == entitlement.Free {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未知档位"})
			return
		}
	case "storage":
		gb, ok := pricing.StorageGBForKey(in.Tier)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未知存储档位"})
			return
		}
		storageGB = gb
	case "credits":
		n, ok := pricing.CreditsForPackKey(in.Tier)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "未知代币档位"})
			return
		}
		credits = n
	}

	id := auth.MustIdentity(c)
	url, err := h.checkoutSvc.Create(c.Request.Context(), id.UserID, in, storageGB, credits)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownSKU):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, ErrCheckoutUnconfigured):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

func NewHandler(svc *Service, checkoutSvc *Checkout, stripeSecret, appleSecret, googlePubsubAud string) *Handler {
	return &Handler{
		svc:             svc,
		checkoutSvc:     checkoutSvc,
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
	if err == nil {
		if err := h.svc.Apply(c.Request.Context(), evt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	if !errors.Is(err, ErrUnknownEvent) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 不是订阅事件，再试一次性支付（代币加购）。
	// 两个 parser 各自只认自己那类事件，都不认就是我们不关心的类型 —— 回 200
	// 让 Stripe 别再重试：webhook 端点默认订阅了一大堆事件类型，
	// 对每个不认识的都回 4xx，Stripe 会把这个端点判为不健康并开始退避。
	purchase, perr := ParseStripeOneTime(body)
	if perr != nil {
		if errors.Is(perr, ErrUnknownEvent) {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": perr.Error()})
		return
	}
	if err := h.svc.ApplyPurchase(c.Request.Context(), purchase); err != nil {
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
