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
	ErrBadToken     = errors.New("upload token invalid")
	ErrTokenExpired = errors.New("upload token expired")
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

// SignUploadToken 产出 `base64url(payload).base64url(mac)`。
//
// 没用 JWT：这里不需要算法协商，而 JWT 的 alg 字段历来是漏洞温床（alg=none、
// HS256/RS256 混淆）。固定 HMAC-SHA256、格式自定，边缘那侧的校验逻辑也就二十行。
func SignUploadToken(secret string, c UploadClaims) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + sign(secret, payload), nil
}

// VerifyUploadToken 校验签名与过期。api 侧 commit 时不重新解令牌
// （意图表才是真相），这个函数主要供测试与本地 dev 边缘桩使用。
func VerifyUploadToken(secret, token string) (*UploadClaims, error) {
	payload, mac, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrBadToken
	}
	if !hmac.Equal([]byte(mac), []byte(sign(secret, payload))) {
		return nil, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrBadToken
	}
	var c UploadClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrBadToken
	}
	if time.Now().Unix() > c.ExpiresAt {
		return nil, ErrTokenExpired
	}
	return &c, nil
}

func sign(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
