package file

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrBadToken     = errors.New("edge token invalid")
	ErrTokenExpired = errors.New("edge token expired")
)

// 令牌用途。参与 HMAC 计算而**不是**写在载荷里靠校验方自觉比对：
// 上传令牌与下载令牌共用同一个 secret，若签名输入不含用途，一个下载令牌
// （只有 key，没有 sz/ch）就能被拿去打 /v1/single —— 边缘会认为「期望 0 字节、
// 无需校验哈希」，于是把那个 key 覆盖成空对象。而 key 是内容寻址的全局去重键，
// 一次覆盖会波及所有引用者。把用途混进签名，跨用途重放在密码学上就不成立，
// 不依赖任何一侧记得写 if claims.typ == ...。
const (
	purposeUpload   = "u"
	purposeDownload = "d"
)

// UploadClaims 是签进上传令牌的全部内容。
//
// 令牌而不是 R2 临时凭证：R2 的 temp credentials 只能 scope 到 bucket/prefix，
// 无法锁死「这一个 key、这么大、这个哈希」。而 storage_key 是内容寻址的
// （= hex(cipher_hash)），一旦客户端能自选 key，它就能拿别人文件的 cipher_hash
// 当 key 去覆盖内容 —— 全局去重意味着那一份字节是所有引用者共享的。
// 所以 key 必须由服务端签死，边缘只认令牌里的值，绝不读 URL 路径。
type UploadClaims struct {
	IntentID   string `json:"iid"`
	Key        string `json:"key"`
	UploadID   string `json:"uid,omitempty"` // multipart 的 uploadId；单片模式为空
	SizeBytes  int64  `json:"sz"`
	PartSize   int64  `json:"ps"`
	PartCount  int    `json:"pc"`
	CipherHash string `json:"ch"` // hex(sha256(密文))；单片模式下边缘据此做整体校验
	ExpiresAt  int64  `json:"exp"`
}

// DownloadClaims 是签进下载令牌的全部内容（ADR-070）。
//
// 同样只签 key，不签 file_id：边缘不认识业务实体，它只需要知道「允许读哪个对象」。
// mime 也签进来，因为直传是用 R2 binding 写的、没带 httpMetadata，边缘无从得知
// 该回什么 Content-Type —— 让它从 R2 元数据读，反而要求上传侧永远记得写对。
type DownloadClaims struct {
	Key       string `json:"key"`
	Mime      string `json:"mime"`
	ExpiresAt int64  `json:"exp"`
}

// SignUploadToken 产出 `base64url(payload).base64url(mac)`。
//
// 没用 JWT：这里不需要算法协商，而 JWT 的 alg 字段历来是漏洞温床（alg=none、
// HS256/RS256 混淆）。固定 HMAC-SHA256、格式自定，边缘那侧的校验逻辑也就二十行。
func SignUploadToken(secret string, c UploadClaims) (string, error) {
	return signToken(secret, purposeUpload, c)
}

// SignDownloadToken 产出下载令牌，格式与上传令牌一致但用途不同（签名不通用）。
func SignDownloadToken(secret string, c DownloadClaims) (string, error) {
	return signToken(secret, purposeDownload, c)
}

// VerifyUploadToken 校验签名与过期。api 侧 commit 时不重新解令牌
// （意图表才是真相），这个函数主要供测试与本地 dev 边缘桩使用。
func VerifyUploadToken(secret, token string) (*UploadClaims, error) {
	var c UploadClaims
	if err := verifyToken(secret, purposeUpload, token, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// VerifyDownloadToken 供内嵌边缘校验下载令牌；生产由 Worker 用同一套算法自行校验。
func VerifyDownloadToken(secret, token string) (*DownloadClaims, error) {
	var c DownloadClaims
	if err := verifyToken(secret, purposeDownload, token, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func signToken(secret, purpose string, claims any) (string, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + sign(secret, purpose, payload), nil
}

// verifyToken 把 payload 解进 out，out 必须是指针。exp 字段各 claims 类型都有，
// 但泛型解出来读不到，所以过期检查放在各自的调用点会重复 —— 这里用一个只含 exp
// 的轻量结构再解一次，多一次 json.Unmarshal 换来过期检查只写一遍。
func verifyToken(secret, purpose, token string, out any) error {
	payload, mac, ok := strings.Cut(token, ".")
	if !ok {
		return ErrBadToken
	}
	if !hmac.Equal([]byte(mac), []byte(sign(secret, purpose, payload))) {
		return ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ErrBadToken
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ErrBadToken
	}
	var exp struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		return ErrBadToken
	}
	if time.Now().Unix() > exp.ExpiresAt {
		return ErrTokenExpired
	}
	return nil
}

func sign(secret, purpose, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(purpose))
	m.Write([]byte("."))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
