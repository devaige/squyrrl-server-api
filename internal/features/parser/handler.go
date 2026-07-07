package parser

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
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
	// 混淆下发（防一眼抓包，非加密，见 manifest_obfuscate.go）。确定性输出，容 CDN 短缓存。
	// 客户端反混淆后读内部 version 决定是否换本地缓存。
	c.Header("Cache-Control", "public, max-age=300")
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
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "余额不足，请充值后重试"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
