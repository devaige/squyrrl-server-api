package file

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

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
	g.GET("/:id/download", h.download)
	g.GET("/:id/thumb", h.thumbnail)
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
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "本服务未启用直传"})
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

// thumbnail 返回服务端预生成的 JPEG 缩略图。
// 没有缩略图（非图片 / 解码失败 / 仍在异步生成中）→ 404，由客户端按需 fallback 到 /download。
func (h *Handler) thumbnail(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	rc, _, err := h.svc.DownloadThumbnail(c.Request.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		case errors.Is(err, ErrNoThumbnail):
			c.JSON(http.StatusNotFound, gin.H{"error": "无缩略图"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	defer rc.Close()

	c.Header("Content-Type", "image/jpeg")
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

func (h *Handler) download(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	rc, f, err := h.svc.Download(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rc.Close()

	c.Header("Content-Type", f.Mime)
	c.Header("Content-Length", strconv.FormatInt(f.SizeBytes, 10))
	c.Header("X-Plain-Hash", f.PlainHash)
	c.Header("X-Cipher-Hash", f.CipherHash)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}
