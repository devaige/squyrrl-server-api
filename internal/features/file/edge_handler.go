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
// 的一份 Go 实现（上传两个端点 + 下载一个），挂在 api 自己身上。
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

// Register 挂在**公开** group：这组端点用边缘令牌认证，不是 Bearer。
func (h *EdgeHandler) Register(g *gin.RouterGroup) {
	g.PUT("/v1/single", h.single)
	g.PUT("/v1/part/:n", h.part)
	g.GET("/v1/blob", h.blob)
}

// bearerOrQuery 取边缘令牌。上传走 Authorization 头，下载走查询串 `?t=`
// （见 service.signBlobURL：URL 要能直接喂给 Image.network），两种都接受。
func bearerOrQuery(c *gin.Context) string {
	if raw := c.GetHeader("Authorization"); len(raw) > 7 && raw[:7] == "Bearer " {
		return raw[7:]
	}
	return c.Query("t")
}

// tokenErrStatus 把令牌错误映射到状态码：密钥没配是服务端问题（503），
// 其余（签名不符、过期、用途不对）都是客户端拿了张不能用的票（401）。
func tokenErrStatus(err error) int {
	if errors.Is(err, ErrEdgeDisabled) {
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}

// claimsFrom 校验上传令牌
func (h *EdgeHandler) claimsFrom(c *gin.Context) (*UploadClaims, bool) {
	tok := bearerOrQuery(c)
	if tok == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少边缘令牌"})
		return nil, false
	}
	claims, err := h.svc.VerifyUploadToken(tok)
	if err != nil {
		c.JSON(tokenErrStatus(err), gin.H{"error": err.Error()})
		return nil, false
	}
	return claims, true
}

// blob 是下载侧的内嵌边缘（ADR-070），对应 Worker 的 GET /v1/blob。
//
// 这里的 io.Copy 确实让字节穿过了 api —— 但这组端点只在非 prod 注册，走的是
// localhost 到 MinIO，不产生任何出网费用。生产上同一条路由由 Worker 承担，
// R2 → Worker → 客户端全程在 Cloudflare 内部。
func (h *EdgeHandler) blob(c *gin.Context) {
	tok := bearerOrQuery(c)
	if tok == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少边缘令牌"})
		return
	}
	claims, err := h.svc.VerifyDownloadToken(tok)
	if err != nil {
		c.JSON(tokenErrStatus(err), gin.H{"error": err.Error()})
		return
	}

	rc, err := h.svc.storage.Get(c.Request.Context(), claims.Key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "对象不存在"})
		return
	}
	defer rc.Close()

	// key 是内容寻址的，同一个 key 的字节永不改变 —— 可以放心 immutable。
	// private 而非 public：URL 里带着令牌，不该被任何共享缓存按 URL 存下来。
	c.Header("Content-Type", claims.Mime)
	c.Header("Cache-Control", "private, max-age=31536000, immutable")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
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
