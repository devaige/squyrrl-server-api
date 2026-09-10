package parser

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/wallet"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/parse", h.parse)
}

// RegisterPublic 挂公开端点。manifest 无鉴权：匿名客户端也要靠它本地判断
// 「该 URI 给『解析』还是『暂存』按钮」，且清单内容非敏感（只是公开 URL 匹配规则）。
func (h *Handler) RegisterPublic(g *gin.RouterGroup) {
	g.GET("/manifest", h.manifest)
}

func (h *Handler) manifest(c *gin.Context) {
	m := h.svc.Manifest()
	plain, err := json.Marshal(m)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "manifest encode failed"})
		return
	}
	// 混淆下发（防一眼抓包，非加密，见 manifest_obfuscate.go）。确定性输出，容 CDN 缓存。
	// 客户端反混淆后读内部 version 决定是否换本地缓存。
	//
	// 4 小时是与边缘对齐的结果，不是随手取的值：这个头决定 Cloudflare 的**边缘** TTL，
	// 而发给浏览器的 max-age 另由 zone 的 Browser Cache TTL 决定，其默认值就是 4 小时且会
	// 覆盖本头。原先这里写 300，于是「代码说 5 分钟、客户端实际缓存 4 小时」——
	// 边缘 TTL 短一点并不能让客户端更快拿到新版本（浏览器那层才是瓶颈），只是白白多打源站。
	// 两边取同一个值，代码与现实一致，也少一次回源。
	//
	// 代价：解析清单变更后，最坏情况客户端约 8 小时（边缘 4h + 浏览器 4h）才看到新版本。
	// 对这份数据可以接受 —— 它只在 parser 的匹配正则变动时才变，而那是低频事件。
	c.Header("Cache-Control", "public, max-age=14400")
	c.String(http.StatusOK, obfuscateManifest(plain))
}

func (h *Handler) parse(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in ParseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Parse(c.Request.Context(), id.UserID, in.URI)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoParser):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, wallet.ErrInsufficientCredits):
			// 与档位门槛共用一套 402 形状：客户端靠 reason 分流（去充值 / 去升级），
			// 而不是匹配文案。文案本身也交还给客户端 —— 它有三语 arb，服务端没有。
			var ie *wallet.InsufficientCreditsError
			if errors.As(err, &ie) {
				c.JSON(http.StatusPaymentRequired, quota.ErrInsufficientCredits(ie.Balance, ie.Required))
			} else {
				c.JSON(http.StatusPaymentRequired, quota.ErrInsufficientCredits(0, 0))
			}
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
