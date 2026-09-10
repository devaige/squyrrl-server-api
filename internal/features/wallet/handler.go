package wallet

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/quota"
)

// StorageReader 提供存储配额与占用。由 quota.Service 实现。
//
// 聚合放在 handler 而不是 Service：quota 构造时需要 wallet（读档位），
// wallet 若反过来在构造期依赖 quota 就成了环。handler 在路由装配阶段才组装，
// 那时两者都已就绪 —— 用构造顺序解开依赖，而不是引入 setter 注入。
type StorageReader interface {
	Storage(ctx context.Context, userID uuid.UUID) (quota.StorageStatus, error)
	CurrentUsage(ctx context.Context, userID uuid.UUID) (quota.Usage, error)
}

type Handler struct {
	svc     *Service
	storage StorageReader
}

func NewHandler(svc *Service, storage StorageReader) *Handler {
	return &Handler{svc: svc, storage: storage}
}

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
	ctx := c.Request.Context()
	w, err := h.svc.GetWallet(ctx, id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	st, err := h.storage.Storage(ctx, id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	use, err := h.storage.CurrentUsage(ctx, id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	w.Storage = StorageView{QuotaBytes: st.QuotaBytes, UsedBytes: st.UsedBytes}
	w.Limits = entitlement.MustOf(w.Plan)
	w.Usage = UsageView{
		Snippets: use.Snippets, Pages: use.Pages,
		Tags: use.Tags, Devices: use.Devices,
	}
	c.JSON(http.StatusOK, w)
}

func (h *Handler) grantCredits(c *gin.Context) {
	var in GrantInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Grant(c.Request.Context(), in.UserID, in.Delta, in.Reason, in.IdempotencyKey)
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
