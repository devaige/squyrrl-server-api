package subscriptions

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Google Play 的凭证和苹果的**完全不是一类东西**。
//
// StoreKit 2 交给客户端的是一张苹果签名过的 JWS：上面写着买了什么、买了几份、
// 属于谁，服务端拿内置的根证书就能独立验完，不联网也成立。Play 交给客户端的
// purchaseToken 只是一串不透明的句柄 —— 它自己**不承载任何信息，也没有签名**。
// 唯一能说明它代表什么的地方是 Play Developer API，所以这条路上服务端必须出网，
// 且「查不到」和「查不通」是两种必须分开处置的失败。
//
// 由此带来的两个直接后果：
//   - 收单要有一份服务账号凭据（比 Apple 多一项部署配置，且它是真正的密钥）；
//   - webhook（RTDN）里的状态字段只当**触发信号**用，真正的状态一律回源去问，
//     因为 RTDN 可能乱序到达、可能重投，而 API 返回的永远是此刻的真相。

const (
	playScope       = "https://www.googleapis.com/auth/androidpublisher"
	playDefaultBase = "https://androidpublisher.googleapis.com"
	playTokenURI    = "https://oauth2.googleapis.com/token"
)

// PlayGuard 是 Google Play 收单的两道闸门，和 AppleGuard 一一对应。
//
// 包名这道闸门在这里比苹果那侧更硬：它是 API 路径的一部分，别的 App 的
// purchaseToken 拿过来直接 404，而不是「验签过了再比对」。
//
// AllowTest 对应苹果的 Sandbox：Play Console 的「许可测试」名单里的账号买东西
// **不扣款**，凭证却完全正常。生产环境放行它等于开了一条免费领权益的口子，
// 而那个名单是可以随时加人的。
type PlayGuard struct {
	PackageName string
	AllowTest   bool
}

func (g PlayGuard) Ready() bool { return g.PackageName != "" }

// playCredentials 服务账号 JSON 里我们用得到的字段。
type playCredentials struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// PlayAPI 是 Play Developer API 的最小客户端：一次 JWT-bearer 换 access token，
// 加两个 GET。
//
// 没有引入 google.golang.org/api/androidpublisher —— 那一支会拖进整个
// googleapis 客户端栈（google-api-go-client + oauth2 + grpc 相关的一大片），
// 而我们实际用到的是两个 REST GET 和一段 RS256 签名，后者仓库里本来就有
// （jws.go 的 Apple/Google 验签）。依赖体量与用量差了两个数量级。
type PlayAPI struct {
	guard    PlayGuard
	email    string
	key      *rsa.PrivateKey
	tokenURI string
	base     string
	http     *http.Client
	now      func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewPlayAPI 从服务账号凭据构造客户端。
//
// raw 既可以是 JSON 原文，也可以是一个文件路径 —— 服务账号 JSON 里的私钥带
// 字面 \n，塞进 .env 的单行值里非常容易在某一层被吃掉一个反斜杠，而那时的
// 表现是「签名算出来了但 Google 说 invalid_grant」。给一条走文件的路，
// 是为了让这类问题有个不必调试的绕法。
//
// 未配置返回 (nil, nil)：调用方用 Ready() 判断，收单入口整体 503。
// 与 AppleGuard 同一个态度 —— 不存在「没配好就退化成不校验」的暗路。
func NewPlayAPI(guard PlayGuard, raw string) (*PlayAPI, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !guard.Ready() {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "{") {
		b, err := os.ReadFile(raw)
		if err != nil {
			return nil, fmt.Errorf("读取 Play 服务账号凭据: %w", err)
		}
		raw = string(b)
	}
	var creds playCredentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return nil, fmt.Errorf("解析 Play 服务账号凭据: %w", err)
	}
	if creds.ClientEmail == "" || creds.PrivateKey == "" {
		return nil, fmt.Errorf("Play 服务账号凭据缺少 client_email / private_key")
	}
	key, err := parseRSAPrivateKey(creds.PrivateKey)
	if err != nil {
		return nil, err
	}
	tokenURI := creds.TokenURI
	if tokenURI == "" {
		tokenURI = playTokenURI
	}
	return &PlayAPI{
		guard:    guard,
		email:    creds.ClientEmail,
		key:      key,
		tokenURI: tokenURI,
		base:     playDefaultBase,
		http:     &http.Client{Timeout: 15 * time.Second},
		now:      time.Now,
	}, nil
}

func (a *PlayAPI) Ready() bool { return a != nil && a.key != nil && a.guard.Ready() }

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, fmt.Errorf("Play 服务账号私钥不是合法 PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("Play 服务账号私钥不是 RSA")
		}
		return rsaKey, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("Play 服务账号私钥解不出: %w", err)
	}
	return k, nil
}

// accessToken 取（并缓存）一枚 androidpublisher 作用域的 access token。
//
// 提前 60 秒过期，是因为「还剩 3 秒」的 token 拿去发请求必然失败一次，
// 而那次失败发生在收单路径上 —— 用户已经付完钱在等结果。
func (a *PlayAPI) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.now().Before(a.tokenExp) {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	a.mu.Unlock()

	assertion, err := a.signAssertion()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", ErrStoreUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// 这里刻意不把 Google 的错误体原样带出去：invalid_grant 的响应里会回显
		// 断言的部分内容。收单失败要留痕，但不该由一条给客户端的错误信息来留。
		return "", fmt.Errorf("play oauth %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", ErrStoreUnavailable
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	a.mu.Lock()
	a.token = out.AccessToken
	a.tokenExp = a.now().Add(ttl - time.Minute)
	a.mu.Unlock()
	return out.AccessToken, nil
}

// signAssertion 签一份 RS256 的服务账号断言（RFC 7523）。
func (a *PlayAPI) signAssertion() (string, error) {
	now := a.now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   a.email,
		"scope": playScope,
		"aud":   a.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// get 发一次已鉴权的 GET，并把 HTTP 状态翻译成两类语义截然不同的错误。
//
// 这个区分是整个文件里最要紧的一处：404/410（商店不认识这个 token）意味着
// 这笔单**永远不会**兑现，客户端应当把它扔掉；5xx / 超时意味着我们暂时问不到，
// 同一个 token 下次还得再问。把后者当成前者，等于把一次网络抖动变成用户付了钱
// 拿不到东西且再也不会重试。
func (a *PlayAPI) get(ctx context.Context, path string, out any) error {
	tok, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(req)
	if err != nil {
		return ErrStoreUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ErrStoreUnavailable
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrUnknownPurchase
	case resp.StatusCode == http.StatusBadRequest:
		// Play 对格式不对、或与商品对不上的 token 回 400。同样是「永远不会成立」。
		return ErrUnknownPurchase
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 我们这边的凭据问题（服务账号没被授权到这个应用），不是用户的问题。
		return ErrPlayUnconfigured
	case resp.StatusCode >= 300:
		return ErrStoreUnavailable
	}
	if err := json.Unmarshal(body, out); err != nil {
		return ErrMalformedBody
	}
	return nil
}

// PlaySubscription 是 purchases.subscriptionsv2.get 的响应里我们用得到的字段。
type PlaySubscription struct {
	SubscriptionState    string `json:"subscriptionState"`
	StartTime            string `json:"startTime"`
	LatestOrderID        string `json:"latestOrderId"`
	AcknowledgementState string `json:"acknowledgementState"`

	// LinkedPurchaseToken 是被这笔购买**取代掉**的那条订阅的 token（升降档、
	// 重新订阅时出现）。Play 不会为被取代的那条单独推一条过期通知 ——
	// 这个字段就是它唯一的死亡证明，不读它那条订阅会永远停在 active。
	//
	// 这在客户端接入换档替换流程之后才真正致命：在那之前用户换档会开出第二条
	// 并行订阅（两笔都扣钱），两条都 active 至少和账单对得上；之后是一笔钱、
	// 两份配额。
	LinkedPurchaseToken string `json:"linkedPurchaseToken"`
	// TestPurchase 只要存在就说明这是许可测试单（结构体内容我们不关心）。
	TestPurchase               *struct{}               `json:"testPurchase"`
	ExternalAccountIdentifiers *PlayExternalAccountIDs `json:"externalAccountIdentifiers"`
	LineItems                  []PlayLineItem          `json:"lineItems"`
}

// PlayExternalAccountIDs 承载订阅侧的账号绑定。整段可能缺席（购买时没传
// obfuscatedAccountId），所以取值走 Obfuscated() 而不是直接点进去。
type PlayExternalAccountIDs struct {
	ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId"`
}

func (e *PlayExternalAccountIDs) Obfuscated() string {
	if e == nil {
		return ""
	}
	return e.ObfuscatedExternalAccountID
}

// PlayLineItem 一份订阅里的一个基础方案。
type PlayLineItem struct {
	ProductID  string `json:"productId"`
	ExpiryTime string `json:"expiryTime"`
}

// PlayProduct 是 purchases.products.get 的响应里我们用得到的字段。
type PlayProduct struct {
	PurchaseTimeMillis int64  `json:"purchaseTimeMillis,string"`
	PurchaseState      int    `json:"purchaseState"` // 0 已购 / 1 已取消 / 2 待处理
	ConsumptionState   int    `json:"consumptionState"`
	OrderID            string `json:"orderId"`
	Quantity           int    `json:"quantity"`
	// PurchaseType 只在非正常购买时出现：0 测试 / 1 促销 / 2 激励视频。
	// 用指针是因为**字段缺席才是正常购买**，零值恰好是「测试」。
	PurchaseType                *int   `json:"purchaseType"`
	ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId"`
}

// Subscription 查一笔订阅。purchaseToken 是路径的一部分，必须转义。
func (a *PlayAPI) Subscription(ctx context.Context, purchaseToken string) (*PlaySubscription, error) {
	if !a.Ready() {
		return nil, ErrPlayUnconfigured
	}
	if purchaseToken == "" {
		return nil, ErrUnknownPurchase
	}
	var out PlaySubscription
	path := fmt.Sprintf("/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		url.PathEscape(a.guard.PackageName), url.PathEscape(purchaseToken))
	if err := a.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Product 查一笔一次性购买。
func (a *PlayAPI) Product(ctx context.Context, productID, purchaseToken string) (*PlayProduct, error) {
	if !a.Ready() {
		return nil, ErrPlayUnconfigured
	}
	if productID == "" || purchaseToken == "" {
		return nil, ErrUnknownPurchase
	}
	var out PlayProduct
	path := fmt.Sprintf("/androidpublisher/v3/applications/%s/purchases/products/%s/tokens/%s",
		url.PathEscape(a.guard.PackageName), url.PathEscape(productID), url.PathEscape(purchaseToken))
	if err := a.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
