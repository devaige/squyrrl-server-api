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
	svc          *Service
	checkoutSvc  *Checkout
	stripeSecret string
	appleGuard   AppleGuard
	googleOIDC   *GoogleOIDCVerifier
}

// RegisterUser 挂用户端购买入口（受 Bearer 保护）。
//
//	POST /me/checkout              发起站外结账（Stripe）
//	POST /me/purchases/app-store   上报一笔已完成的 App Store 内购
//
// 两条路的方向是相反的：结账是「我们把用户送去付款」，上报是「用户在别处付完了
// 回来兑现」。原生内购只能是后者 —— 支付在 StoreKit 里闭环完成，服务端事后才
// 从一张凭证上知道发生过什么。
func (h *Handler) RegisterUser(g *gin.RouterGroup) {
	g.POST("/checkout", h.checkout)
	g.POST("/purchases/app-store", h.appStorePurchase)
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

func NewHandler(
	svc *Service,
	checkoutSvc *Checkout,
	stripeSecret string,
	appleGuard AppleGuard,
	googleOIDC *GoogleOIDCVerifier,
) *Handler {
	return &Handler{
		svc:          svc,
		checkoutSvc:  checkoutSvc,
		stripeSecret: stripeSecret,
		appleGuard:   appleGuard,
		googleOIDC:   googleOIDC,
	}
}

// AppStorePurchaseRequest 客户端上报的内购凭证。
//
// 只收一个 JWS，**不收 productId、数量、金额**：凭证本身是苹果签名过的，
// 上面写着买了什么、买了几份；客户端另外报一遍只能制造两个信息源，
// 而其中一个是攻击者能改的那个。
type AppStorePurchaseRequest struct {
	SignedTransaction string `json:"signed_transaction" binding:"required"`
}

// appStorePurchase 校验并兑现一笔 App Store 内购。
func (h *Handler) appStorePurchase(c *gin.Context) {
	if !h.appleGuard.Ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": ErrAppleUnconfigured.Error()})
		return
	}
	var in AppStorePurchaseRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now()
	txn, err := VerifyAppleTransaction(in.SignedTransaction, now)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.appleGuard.Check(txn); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 这一步是整条路上最吃紧的一道闸门。凭证是苹果签的，**但苹果不知道
	// Squyrrl 的账号体系**：验签只证明「这是一笔真实的内购」，不证明它是
	// 眼前这个登录用户买的。少了这行比对，任何人拿到任意一张本 App 的有效
	// 凭证（自己账号的旧凭证、别人分享的、越狱设备上抓的）都能反复兑现到
	// 自己名下。appAccountToken 由客户端在发起购买时写入，是两套身份之间
	// 唯一的绑定点。
	id := auth.MustIdentity(c)
	uid, err := txn.UserID()
	if err != nil || uid != id.UserID {
		c.JSON(http.StatusForbidden, gin.H{"error": ErrForeignReceipt.Error()})
		return
	}

	evt, err := AppleEventFrom(txn, "", now)
	if err != nil {
		if errors.Is(err, ErrUnknownEvent) {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.applyApple(c, evt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// applyApple 把归一化后的苹果事件落库。两条分支的幂等各自由下游保证：
// 订阅按 (provider, originalTransactionId) upsert，代币按 transactionId 去重 ——
// 客户端重启后会把还没 finish 的交易**重新上报一遍**，重复到达是常态。
func (h *Handler) applyApple(c *gin.Context, evt *AppleEvent) error {
	if evt.Purchase != nil {
		return h.svc.ApplyPurchase(c.Request.Context(), evt.Purchase)
	}
	return h.svc.Apply(c.Request.Context(), evt.Subscription)
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
	now := time.Now()
	evt, err := ParseAppleNotification(body, h.appleGuard, now)
	if err != nil {
		// 认不出的通知类型回 200：苹果对 4xx 会持续重投，最终把端点标为不可达。
		// 「我们不关心这个类型」不是一种失败。
		if errors.Is(err, ErrUnknownEvent) {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.applyApple(c, evt); err != nil {
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
	if err := h.googleOIDC.Verify(c.Request.Context(), c.GetHeader("Authorization")); err != nil {
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
