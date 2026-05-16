package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
)

const (
	// 32 字节随机熵 → base64-url 后约 43 字符的不透明 token
	opaqueTokenBytes = 32
)

// newOpaqueToken 生成一个 256-bit 熵的不透明字符串 token
// access / refresh token 都用它，token 永远不入库，只入库 SHA-256 哈希
func newOpaqueToken() (string, error) {
	b := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newOTPCode 生成 6 位数字 OTP，前导补零
func newOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashSHA256 用于存储 token / OTP 的服务端哈希
func hashSHA256(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
