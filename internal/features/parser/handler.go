package parser

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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

// RegisterInternal 挂后台端点（/internal/admin，受 X-Internal-Token 保护）：解析记录与平台售价。
func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.GET("/parse-logs", h.listLogs)
	g.GET("/parse-logs/summary", h.logSummary)
	g.GET("/parse-prices", h.listPrices)
	g.PUT("/parse-prices/:provider", h.setPrice)
	g.DELETE("/parse-prices/:provider", h.clearPrice)
}

func (h *Handler) manifest(c *gin.Context) {
	m, err := h.svc.Manifest(c.Request.Context())
	if err != nil {
		// 客户端拿不到新清单会继续用本地那份，比下发一份残缺的清单安全
		slog.Warn("解析清单生成失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "manifest unavailable"})
		return
	}
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
	// 对这份数据可以接受 —— 它只在匹配正则变动、或某个平台的付费接口整体上下线时才变，
	// 都是低频事件；过渡期内客户端对已下线平台发起的解析会失败，但失败不扣费。
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
	res, err := h.svc.Parse(c.Request.Context(), id.UserID, in)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoParser):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, ErrParseFailed):
			// 只回一句通用原因：具体是哪个来源、为什么失败在解析记录里，不能让用户看到
			c.JSON(http.StatusBadGateway, gin.H{"error": ErrParseFailed.Error()})
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
			slog.Warn("解析请求出错", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}

// listLogs 支持 provider / status / source 筛选；翻页用上一页最后一条的 before（RFC3339Nano）+ before_id。
func (h *Handler) listLogs(c *gin.Context) {
	f := LogFilter{
		Provider: c.Query("provider"),
		Status:   c.Query("status"),
		Source:   c.Query("source"),
	}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if b, bid := c.Query("before"), c.Query("before_id"); b != "" || bid != "" {
		t, err1 := time.Parse(time.RFC3339Nano, b)
		id, err2 := uuid.Parse(bid)
		if err1 != nil || err2 != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "before 与 before_id 须同时给出"})
			return
		}
		f.Before, f.BeforeID = &t, &id
	}
	items, err := h.svc.Logs(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) logSummary(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	if days <= 0 || days > 366 {
		days = 30
	}
	providers, sources, err := h.svc.LogSummary(c.Request.Context(), days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"days": days, "providers": providers, "sources": sources})
}

func (h *Handler) listPrices(c *gin.Context) {
	items, err := h.svc.Prices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) setPrice(c *gin.Context) {
	var in struct {
		Credits int64 `json:"credits"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.SetPrice(c.Request.Context(), c.Param("provider"), in.Credits); err != nil {
		switch {
		case errors.Is(err, ErrNoParser):
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider"})
		case errors.Is(err, ErrBadPrice):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) clearPrice(c *gin.Context) {
	if err := h.svc.ClearPrice(c.Request.Context(), c.Param("provider")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
