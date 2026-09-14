package subscriptions

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 三家渠道最终都在签同一种东西：一个 JWS。Apple 用 ES256 并把整条证书链塞在
// 头部的 x5c 里自证，Google Pub/Sub 用 RS256 + 一份在线 JWKS。拆 token、卡死
// 算法、看有效期这部分共用，各自只保留「公钥从哪来」那一步。

// jwsHeader 只解我们用得到的字段。**头部是攻击者能改的部分**，这里的任何值都
// 不能拿来决定验签方式，只能拿来定位公钥。
type jwsHeader struct {
	Alg string   `json:"alg"`
	Kid string   `json:"kid"`
	X5c []string `json:"x5c"`
}

// splitJWS 拆开 compact serialization 的三段。**它不做任何验签** ——
// 返回的 payload 在调用方验完签之前是纯粹的攻击者输入。
func splitJWS(tok string) (h jwsHeader, payload, signingInput, sig []byte, err error) {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return h, nil, nil, nil, ErrBadSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return h, nil, nil, nil, ErrBadSignature
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, nil, nil, nil, ErrBadSignature
	}
	if payload, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
		return h, nil, nil, nil, ErrBadSignature
	}
	if sig, err = base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return h, nil, nil, nil, ErrBadSignature
	}
	// 签名覆盖的是**编码后的原文**，不是我们解码再重编出来的字节：
	// base64 有多种等价写法，重编一次就可能和签名时的字节不同。
	return h, payload, []byte(parts[0] + "." + parts[1]), sig, nil
}

// ─────────────────────────── Apple：ES256 + x5c ───────────────────────────

//go:embed apple_root_ca_g3.pem
var appleRootPEM []byte

// appleRoots 是校验 App Store 签名的唯一信任锚。
//
// 内置而不是放环境变量：它是一条「这个签名到底是不是苹果发的」的判据，不是部署
// 差异。放进 env 意味着运维填错一个字符就静默退化成「谁签的都认」，而那正是这
// 整套验签要防的事。指纹由 jws_test.go 锁死，防止文件被无意改动。
var appleRoots = func() *x509.CertPool {
	blk, _ := pem.Decode(appleRootPEM)
	if blk == nil {
		panic("subscriptions: 内置的 Apple 根证书解不出 PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		panic("subscriptions: 内置的 Apple 根证书解不出 X.509: " + err.Error())
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}()

// VerifyAppleJWS 校验一段 App Store 签发的 JWS，返回它的 payload。
//
// now 显式传入而不是取 time.Now()：证书链校验对时间敏感，测试要能造出
// 「证书还没生效 / 已经过期」两种情形。
func VerifyAppleJWS(tok string, now time.Time) ([]byte, error) {
	h, payload, signingInput, sig, err := splitJWS(tok)
	if err != nil {
		return nil, err
	}
	// 算法写死，不按头部里的 alg 选：那是 JWT 最经典的一类漏洞
	// （alg:none 直接免签；把 RS256 换成 HS256 让验签方拿公钥当 HMAC 密钥用，
	// 而公钥是公开的）。头部由签名覆盖不代表可信 —— 验签之前它还没被覆盖。
	if h.Alg != "ES256" {
		return nil, ErrBadSignature
	}
	if len(h.X5c) < 2 {
		return nil, ErrBadSignature
	}
	chain := make([]*x509.Certificate, 0, len(h.X5c))
	for _, b64 := range h.X5c {
		// x5c 用的是**标准** base64（RFC 7515 §4.1.6），与 token 三段的 base64url
		// 不是一回事；用错解码器的表现是链解不出来，报错却指向证书本身。
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, ErrBadSignature
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, ErrBadSignature
		}
		chain = append(chain, cert)
	}

	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         appleRoots,
		Intermediates: inter,
		CurrentTime:   now,
		// 苹果这条链的叶证书带的是自定义扩展（1.2.840.113635.100.6.11.1）而不是
		// 标准 EKU，按默认的 ServerAuth 去卡会一律失败。
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, ErrBadSignature
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, ErrBadSignature
	}
	if !verifyES256(pub, signingInput, sig) {
		return nil, ErrBadSignature
	}
	return payload, nil
}

// verifyES256 验 JWS 形态的 ECDSA 签名。
//
// JWS 的签名是定长的 R‖S 拼接（RFC 7518 §3.4），**不是** DER 包装的 ASN.1。
// 顺手用 ecdsa.VerifyASN1 会 100% 失败，且失败得毫无线索 —— 它只会返回 false。
func verifyES256(pub *ecdsa.PublicKey, signingInput, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256(signingInput)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, sum[:], r, s)
}

// ─────────────────────────── Google：RS256 + JWKS ───────────────────────────

// KeySource 按 kid 取一把 RSA 公钥。抽成接口只为一件事：让验签逻辑能在没有网络
// 的测试里跑完整条路径，而不是只测「解析 claims」这半截。
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// GoogleOIDCVerifier 校验 Cloud Pub/Sub push 请求带的 OIDC token。
//
// 这里此前只解 payload 比对 aud、**完全不验签名**。那意味着 /webhooks/google
// 上任何人构造一个 aud 对得上的 base64 串就能给自己开任意档位的订阅 ——
// 端点挂在公网根上，连 Bearer 中间件都没有。
type GoogleOIDCVerifier struct {
	aud   string
	email string // 可选：Pub/Sub 订阅配置的服务账号邮箱，配了就必须对上
	keys  KeySource
	now   func() time.Time
}

func NewGoogleOIDCVerifier(aud, email string, keys KeySource) *GoogleOIDCVerifier {
	if keys == nil {
		keys = NewGoogleJWKS("")
	}
	return &GoogleOIDCVerifier{aud: aud, email: email, keys: keys, now: time.Now}
}

// Enabled 未配 audience 即视为未启用。fail-closed：Verify 一律拒绝。
func (v *GoogleOIDCVerifier) Enabled() bool { return v != nil && v.aud != "" }

func (v *GoogleOIDCVerifier) Verify(ctx context.Context, authorization string) error {
	if !v.Enabled() {
		return ErrBadSignature
	}
	tok := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	h, payload, signingInput, sig, err := splitJWS(tok)
	if err != nil {
		return err
	}
	if h.Alg != "RS256" {
		return ErrBadSignature
	}
	pub, err := v.keys.Key(ctx, h.Kid)
	if err != nil {
		return ErrBadSignature
	}
	sum := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return ErrBadSignature
	}

	var claims struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Exp           int64  `json:"exp"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ErrBadSignature
	}
	if claims.Iss != "https://accounts.google.com" && claims.Iss != "accounts.google.com" {
		return ErrBadSignature
	}
	if claims.Aud != v.aud {
		return ErrBadSignature
	}
	if claims.Exp <= 0 || v.now().Unix() > claims.Exp {
		return ErrBadSignature
	}
	// 光验签不够：这把签名的密钥是 Google 给**所有** OIDC token 用的同一套。
	// 不比对签发对象，任何 Google 账号自己签出来的 token 都能进来。
	if v.email != "" && (!claims.EmailVerified || claims.Email != v.email) {
		return ErrBadSignature
	}
	return nil
}

// googleJWKSURL 是 Google 公开的 OIDC 验签公钥集。
const googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

// GoogleJWKS 带缓存地取 Google 的验签公钥。
type GoogleJWKS struct {
	url  string
	http *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	ttl       time.Duration
	// minRefetch 限制「认不出 kid 就重取」的频率。Google 会轮换密钥，认不出的
	// kid 多半意味着该刷新了；但也可能是伪造的 token 在拿我们当代理去打 Google。
	minRefetch time.Duration
}

func NewGoogleJWKS(url string) *GoogleJWKS {
	if url == "" {
		url = googleJWKSURL
	}
	return &GoogleJWKS{
		url:        url,
		http:       &http.Client{Timeout: 10 * time.Second},
		keys:       map[string]*rsa.PublicKey{},
		ttl:        time.Hour,
		minRefetch: time.Minute,
	}
}

func (j *GoogleJWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, ErrBadSignature
	}
	j.mu.Lock()
	k, ok := j.keys[kid]
	age := time.Since(j.fetchedAt)
	j.mu.Unlock()
	if ok && age < j.ttl {
		return k, nil
	}
	if !ok && age < j.minRefetch {
		return nil, ErrBadSignature
	}

	fresh, err := j.fetch(ctx)
	if err != nil {
		// 取不到就用旧的顶一会儿：Google 侧一次抖动不该让所有续期事件被拒 ——
		// 被拒的事件 Pub/Sub 会重投，但反复重投同样会把端点判为不健康。
		if ok {
			return k, nil
		}
		return nil, err
	}
	j.mu.Lock()
	j.keys = fresh
	j.fetchedAt = time.Now()
	k, ok = j.keys[kid]
	j.mu.Unlock()
	if !ok {
		return nil, ErrBadSignature
	}
	return k, nil
}

func (j *GoogleJWKS) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("google jwks %d", resp.StatusCode)
	}
	return parseJWKS(body)
}

// parseJWKS 解 RFC 7517 的密钥集，只收 RS256 的 RSA 公钥。
func parseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, ErrMalformedBody
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 8 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(out) == 0 {
		return nil, ErrMalformedBody
	}
	return out, nil
}
