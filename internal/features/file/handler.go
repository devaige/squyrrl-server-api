package file

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/check", h.check)
	g.POST("", h.upload)
	g.GET("/:id", h.metadata)
	g.GET("/:id/download", h.download)
	g.GET("/:id/thumb", h.thumbnail)
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

func (h *Handler) upload(c *gin.Context) {
	plainHashHex := c.GetHeader("X-Plain-Hash")
	cipherHashHex := c.GetHeader("X-Cipher-Hash")
	mime := c.GetHeader("X-Mime")
	if mime == "" {
		mime = "application/octet-stream"
	}

	plain, err := hex.DecodeString(plainHashHex)
	if err != nil || len(plain) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Plain-Hash 缺失或非法（需 SHA-256 hex）"})
		return
	}
	cipher, err := hex.DecodeString(cipherHashHex)
	if err != nil || len(cipher) != 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Cipher-Hash 缺失或非法"})
		return
	}

	size, err := strconv.ParseInt(c.GetHeader("X-Size-Bytes"), 10, 64)
	if err != nil || size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Size-Bytes 缺失或非法"})
		return
	}
	if size > MaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "单文件超过 100MiB 上限"})
		return
	}

	f, err := h.svc.Upload(c.Request.Context(), plain, cipher, size, mime, c.Request.Body)
	if err != nil {
		switch {
		case errors.Is(err, ErrCipherMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "上传字节哈希与 X-Cipher-Hash 不匹配"})
		case errors.Is(err, ErrSizeMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": "上传字节大小与 X-Size-Bytes 不匹配"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, UploadResponse{File: f})
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
