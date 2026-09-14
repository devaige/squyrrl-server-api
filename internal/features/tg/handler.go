package tg

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/quota"
)

type Handler struct {
	svc     *Service
	fileSvc *file.Service
}

func NewHandler(svc *Service, fileSvc *file.Service) *Handler {
	return &Handler{svc: svc, fileSvc: fileSvc}
}

// =============================================================================
// 用户端：受 Bearer 中间件保护，挂在 /me/tg
// =============================================================================

func (h *Handler) RegisterUser(g *gin.RouterGroup) {
	g.POST("/binding/link", h.issueLink)
	g.GET("/binding/pending", h.pendingClaim)
	g.POST("/binding/confirm", h.confirmClaim)
	g.POST("/binding/reject", h.rejectClaim)
	g.GET("/bindings", h.listBindings)
	g.DELETE("/bindings/:id", h.unbind)
}

func (h *Handler) issueLink(c *gin.Context) {
	id := auth.MustIdentity(c)
	link, err := h.svc.IssueBindingLink(c.Request.Context(), id.UserID)
	if err != nil {
		// 402 要在这里就能出去：客户端据它把用户带去商品页，而不是让他走完
		// 「扫码 → 发短码 → 回来确认」再撞墙。
		if quota.WriteIfQuota(c, err) {
			return
		}
		if errors.Is(err, ErrBotUnconfigured) {
			// 503 而非 500：这是部署缺一个环境变量，不是代码出错，
			// 客户端据此提示「功能暂未开放」而不是「出错了，重试」。
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, link)
}

// pendingClaim 被绑定面板每隔几秒轮询一次。「没有待确认」是最常见的结果，
// 所以它是 200 + null 而不是 404 —— 404 会让客户端的错误拦截器（以及日志）
// 把一条完全正常的轮询当成异常。
func (h *Handler) pendingClaim(c *gin.Context) {
	id := auth.MustIdentity(c)
	p, err := h.svc.PendingBindingClaim(c.Request.Context(), id.UserID)
	if err != nil {
		if errors.Is(err, ErrNoPendingClaim) {
			c.JSON(http.StatusOK, gin.H{"pending": nil})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pending": p})
}

func (h *Handler) confirmClaim(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in ConfirmClaimInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	b, err := h.svc.ConfirmBindingClaim(c.Request.Context(), id.UserID, in.PlatformUserID)
	if err != nil {
		if quota.WriteIfQuota(c, err) {
			return
		}
		switch {
		case errors.Is(err, ErrNoPendingClaim):
			c.JSON(http.StatusGone, gin.H{"error": err.Error()})
		case errors.Is(err, ErrAlreadyBound):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, b)
}

func (h *Handler) rejectClaim(c *gin.Context) {
	id := auth.MustIdentity(c)
	if err := h.svc.RejectBindingClaim(c.Request.Context(), id.UserID); err != nil {
		if errors.Is(err, ErrNoPendingClaim) {
			c.Status(http.StatusNoContent) // 已经没有待确认的了，用户要的结果已达成
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) listBindings(c *gin.Context) {
	id := auth.MustIdentity(c)
	list, err := h.svc.ListBindings(c.Request.Context(), id.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"bindings": list})
}

func (h *Handler) unbind(c *gin.Context) {
	id := auth.MustIdentity(c)
	bindingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	if err := h.svc.Unbind(c.Request.Context(), id.UserID, bindingID); err != nil {
		if errors.Is(err, ErrBindingNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// =============================================================================
// Bot 内部端：受 X-Internal-Token 头保护
// =============================================================================

func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.POST("/binding/consume", h.consumeToken)
	g.POST("/binding/claim", h.claimCode)
	g.POST("/binding/revoke", h.revokeBinding)
	g.GET("/binding/status", h.bindingStatus)
	g.POST("/snippet", h.forwardSnippet)
	// 文件走内部端而非 /files：那组挂着 Bearer 中间件，而 Bot 只有内部 token，
	// 拿不到用户身份。files 表本身是全局去重的（ADR-004），与 user 无关，
	// 所以这里只是换一道守卫复用同一个 file.Service，没有第二套存储逻辑。
	g.POST("/files/check", h.checkFile)
	g.POST("/files", h.uploadFile)
}

func (h *Handler) consumeToken(c *gin.Context) {
	var in ConsumeTokenInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	b, err := h.svc.Bind(c.Request.Context(), in.Token, in.TGIdentity)
	if err != nil {
		// 档位门槛要先于 default 分支判断，否则「绑定数已达上限」会以 500 出去，
		// Bot 那侧看到的是「服务端炸了」而不是一句能转述给用户的话。
		if quota.WriteIfQuota(c, err) {
			return
		}
		switch {
		case errors.Is(err, ErrTokenInvalid):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, ErrAlreadyBound):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, b)
}

// claimCode 是短码路径的第一步。它**不建立绑定**，所以这里不会出现 402：
// 档位门槛留到用户在 App 里点确认那一刻，那时才知道要往谁的账户上绑。
func (h *Handler) claimCode(c *gin.Context) {
	var in ClaimCodeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	exp, err := h.svc.ClaimBindingCode(c.Request.Context(), in.Code, in.TGIdentity)
	if err != nil {
		switch {
		case errors.Is(err, ErrTooManyAttempts):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		case errors.Is(err, ErrCodeInvalid):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, ClaimCodeResponse{ExpiresAt: exp})
}

func (h *Handler) revokeBinding(c *gin.Context) {
	var in RevokeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.UnbindByTGUser(c.Request.Context(), in.TGUserID); err != nil {
		if errors.Is(err, ErrNotBound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) bindingStatus(c *gin.Context) {
	tgUserID, err := strconv.ParseInt(c.Query("tg_user_id"), 10, 64)
	if err != nil || tgUserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tg_user_id 缺失或非法"})
		return
	}
	res, err := h.svc.Status(c.Request.Context(), tgUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) forwardSnippet(c *gin.Context) {
	var in SnippetForwardInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.CreateForwardedSnippet(c.Request.Context(), in.TGUserID, &in.Snippet)
	if err != nil {
		if errors.Is(err, ErrNotBound) {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, res)
}

// =============================================================================
// 文件：与 /files 的用户端契约保持一致，只是换一道守卫
// =============================================================================

func (h *Handler) checkFile(c *gin.Context) {
	var in file.CheckInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	plain, err := hex.DecodeString(in.PlainHash)
	if err != nil || len(plain) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plain_hash 必须是 64 位 hex 的 SHA-256"})
		return
	}
	f, exists, err := h.fileSvc.Check(c.Request.Context(), plain)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, file.CheckResponse{Exists: exists, File: f})
}

func (h *Handler) uploadFile(c *gin.Context) {
	plain, err := hex.DecodeString(c.GetHeader("X-Plain-Hash"))
	if err != nil || len(plain) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Plain-Hash 缺失或非法（需 SHA-256 hex）"})
		return
	}
	cipher, err := hex.DecodeString(c.GetHeader("X-Cipher-Hash"))
	if err != nil || len(cipher) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Cipher-Hash 缺失或非法"})
		return
	}
	size, err := strconv.ParseInt(c.GetHeader("X-Size-Bytes"), 10, 64)
	if err != nil || size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Size-Bytes 缺失或非法"})
		return
	}
	if size > file.MaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "单文件超过 100MiB 上限"})
		return
	}
	mime := c.GetHeader("X-Mime")
	if mime == "" {
		mime = "application/octet-stream"
	}

	// 配额卡在读取请求体**之前**：读完再拒等于白白吃下一次入网传输，
	// 而这条路径正是唯一还在消耗服务器带宽的那条。
	//
	// 头缺失一律拒绝，**不再「缺省即跳过」**。原先那版 fail-open 是为了兼容
	// 尚未升级的 Bot，但两者的代价完全不对称：fail-closed 坏掉的窗口里 TG 文件传不上来
	// （用户看得见、一次部署就能修），fail-open 坏掉的窗口里配额**根本不存在**
	// 而一切正常 —— 字节照落 R2、照按月计费，没有任何信号。
	// 而且 ADR-063 让 Bot 与 api 各自独立部署，这个窗口可以由一次回滚随时重开。
	tgUserID, err := strconv.ParseInt(c.GetHeader("X-TG-User-ID"), 10, 64)
	if err != nil || tgUserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-TG-User-ID 缺失或非法"})
		return
	}
	if err := h.svc.CheckUploadFor(c.Request.Context(), tgUserID, size); err != nil {
		if quota.WriteIfQuota(c, err) {
			return
		}
		if errors.Is(err, ErrNotBound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	f, err := h.fileSvc.Upload(c.Request.Context(), plain, cipher, size, mime, c.Request.Body)
	if err != nil {
		switch {
		case errors.Is(err, file.ErrCipherMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "上传字节哈希与 X-Cipher-Hash 不匹配"})
		case errors.Is(err, file.ErrSizeMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "上传字节大小与 X-Size-Bytes 不匹配"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, file.UploadResponse{File: f})
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
