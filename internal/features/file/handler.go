package file

import (
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/check", h.check)
	// 直传两步（ADR-069）：签发 → 客户端把字节分片送进边缘 Worker → 收尾
	g.POST("/intent", h.intent)
	g.POST("/commit", h.commit)
	g.GET("/:id", h.metadata)
	// 下载票据（ADR-070）：回一组短期边缘 URL，字节不经 api
	g.GET("/:id/ticket", h.ticket)
}

// intent 签发直传令牌。命中全局去重时回包只有 exists+file，客户端一个字节都不用传。
func (h *Handler) intent(c *gin.Context) {
	id := auth.MustIdentity(c)

	var in IntentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	plain, err := hex.DecodeString(in.PlainHash)
	if err != nil || len(plain) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plain_hash 必须是 64 位 hex 的 SHA-256"})
		return
	}
	cipher, err := hex.DecodeString(in.CipherHash)
	if err != nil || len(cipher) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cipher_hash 必须是 64 位 hex 的 SHA-256"})
		return
	}
	if in.SizeBytes > MaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "单文件超过 100MiB 上限"})
		return
	}
	mime := in.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}

	resp, err := h.svc.IssueIntent(c.Request.Context(), id.UserID, plain, cipher, in.SizeBytes, mime)
	if err != nil {
		if errors.Is(err, ErrEdgeDisabled) {
			// 503 而不是 500：这是配置缺失，不是偶发故障，重试不会好。
			// 客户端此时**没有**回退路径 —— 中转上传已随 ADR-069 删除，
			// 这正是想要的：漏配的后果是传不了文件，而不是账单上多一笔出网流量。
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "本服务未启用边缘"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// commit 收尾直传：合并分片、核对 R2 实际字节数、写 files 行。
func (h *Handler) commit(c *gin.Context) {
	id := auth.MustIdentity(c)

	var in CommitInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	iid, err := uuid.Parse(in.IntentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 intent_id"})
		return
	}

	f, err := h.svc.Commit(c.Request.Context(), id.UserID, iid, in.Parts)
	if err != nil {
		switch {
		case errors.Is(err, ErrIntentNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "上传意图不存在或不属于当前用户"})
		case errors.Is(err, ErrIntentState):
			c.JSON(http.StatusConflict, gin.H{"error": "该上传意图已收尾或已作废"})
		case errors.Is(err, ErrPartsMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "分片清单与签发时的数量/序号不符"})
		case errors.Is(err, ErrSizeMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "R2 中的实际字节数与申请时声明的不符"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, UploadResponse{File: f})
}

func (h *Handler) metadata(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	f, err := h.svc.GetMetadata(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, f)
}

// =============================================================================
// 端点
// =============================================================================

func (h *Handler) check(c *gin.Context) {
	var in CheckInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	plain, err := hex.DecodeString(in.PlainHash)
	if err != nil || len(plain) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plain_hash 必须是 64 位 hex 的 SHA-256"})
		return
	}

	f, exists, err := h.svc.Check(c.Request.Context(), plain)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, CheckResponse{Exists: exists, File: f})
}

// ticket 签发短期边缘直读 URL（ADR-070）。取代了此前 io.Copy 字节的
// GET /:id/download 与 /:id/thumb —— 那两个端点每次读都要让整个对象穿过 api，
// 而读的次数远多于写，是三条服务器出网通道里最贵的一条。
//
// 回包里 thumb_url 缺省即「没有缩略图」，客户端据此决定回退到原图，
// 不必再打一次 /thumb 去吃 404。
func (h *Handler) ticket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	t, err := h.svc.IssueTicket(c.Request.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		case errors.Is(err, ErrEdgeDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "本服务未启用边缘"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, t)
}
