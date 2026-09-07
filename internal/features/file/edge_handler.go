package file

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// EdgeHandler 是内嵌边缘：与 server/edge/upload 的 Cloudflare Worker **契约完全一致**
// 的一份 Go 实现，挂在 api 自己身上。
//
// 存在的理由是 dev：本地接 MinIO，而 Worker 的 R2 binding 连不到 MinIO
// （`wrangler dev` 的本地 R2 是另一套存储，commit 时 StatObject 必然找不到对象）。
// 没有它，删掉中转端点后 dev 环境就完全传不了文件。
//
// 关键收益是客户端**只有一条上传代码路径**：dev 与生产的唯一差别是 intent 回包里的
// upload_url 指向谁。旧的 POST /files 是另一套语义（header 申报 + 服务端 check + insert），
// 留着就等于让客户端长期维护两条分支。
type EdgeHandler struct {
	svc *Service
}

func NewEdgeHandler(svc *Service) *EdgeHandler { return &EdgeHandler{svc: svc} }

// Register 挂在**公开** group：这组端点用上传令牌认证，不是 Bearer。
func (h *EdgeHandler) Register(g *gin.RouterGroup) {
	g.PUT("/v1/single", h.single)
	g.PUT("/v1/part/:n", h.part)
}

// claimsFrom 校验 Authorization 头里的上传令牌
func (h *EdgeHandler) claimsFrom(c *gin.Context) (*UploadClaims, bool) {
	raw := c.GetHeader("Authorization")
	if len(raw) < 8 || raw[:7] != "Bearer " {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少 Bearer 令牌"})
		return nil, false
	}
	claims, err := h.svc.VerifyUploadToken(raw[7:])
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrEdgeDisabled) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return nil, false
	}
	return claims, true
}

// readAndVerify 读满一片并校验大小与哈希。与 Worker 侧同名函数一一对应：
// 校验通过才写存储，失败路径不留下任何字节。
func readAndVerify(c *gin.Context, expectSize int64, expectHash string) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, expectSize+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, false
	}
	if int64(len(data)) != expectSize {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "分片大小不符：收到 " + strconv.Itoa(len(data)) +
				"，期望 " + strconv.FormatInt(expectSize, 10)})
		return nil, false
	}
	if expectHash != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != expectHash {
			c.JSON(http.StatusBadRequest, gin.H{"error": "分片哈希不符，疑似传输损坏"})
			return nil, false
		}
	}
	return data, true
}

func (h *EdgeHandler) single(c *gin.Context) {
	claims, ok := h.claimsFrom(c)
	if !ok {
		return
	}
	if claims.UploadID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该令牌是 multipart 模式，应调用 /v1/part/{n}"})
		return
	}

	// 单片模式一次请求就是全部字节，故这里能校验整体 cipher_hash
	data, ok := readAndVerify(c, claims.SizeBytes, claims.CipherHash)
	if !ok {
		return
	}
	if err := h.svc.storage.Put(c.Request.Context(), claims.Key,
		bytes.NewReader(data), claims.SizeBytes, "application/octet-stream"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *EdgeHandler) part(c *gin.Context) {
	claims, ok := h.claimsFrom(c)
	if !ok {
		return
	}
	if claims.UploadID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该令牌是单片模式，应调用 /v1/single"})
		return
	}
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 || n > claims.PartCount {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分片序号越界"})
		return
	}

	// 最后一片是余数，其余恒为 PartSize
	expectSize := claims.PartSize
	if n == claims.PartCount {
		expectSize = claims.SizeBytes - claims.PartSize*int64(claims.PartCount-1)
	}

	// multipart 下整体哈希跨请求算不出来，改为逐片校验客户端声明的片哈希
	data, ok := readAndVerify(c, expectSize, c.GetHeader("X-Part-Hash"))
	if !ok {
		return
	}

	etag, err := h.svc.storage.PutPart(c.Request.Context(), claims.Key, claims.UploadID,
		n, bytes.NewReader(data), expectSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"part_number": n, "etag": etag})
}
