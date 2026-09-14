package subscriptions

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────── 内置根证书 ───────────────────────────

// 这个指纹是 Apple Root CA - G3 的公开指纹。锁住它，是因为 apple_root_ca_g3.pem
// 是整套 App Store 收单的唯一信任锚：文件被换成别的证书，验签会照常「通过」，
// 只是从此认的是别人的签名。没有哪个业务测试会因此变红。
const appleRootSHA256 = "63343abfb89a6a03ebb57e9b3f5fa7be7c4f5c756f3017b3a8c488c3653e9179"

func TestEmbeddedAppleRootIsTheRealOne(t *testing.T) {
	blk, _ := pem.Decode(appleRootPEM)
	if blk == nil {
		t.Fatal("内置根证书解不出 PEM")
	}
	sum := sha256.Sum256(blk.Bytes)
	if got := hex.EncodeToString(sum[:]); got != appleRootSHA256 {
		t.Fatalf("内置根证书指纹变了：%s", got)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解不出 X.509：%v", err)
	}
	if cert.Subject.CommonName != "Apple Root CA - G3" {
		t.Errorf("subject CN = %q", cert.Subject.CommonName)
	}
}

// ─────────────────────────── 测试用证书链 ───────────────────────────

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func mkCert(t *testing.T, cn string, isCA bool, parent *testCert, nb, na time.Time) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             nb,
		NotAfter:              na,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signCert, signKey := tmpl, key
	if parent != nil {
		signCert, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signCert, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: cert, key: key, der: der}
}

// fakeAppleChain 造一条 root → intermediate → leaf，并把 root 临时装进信任锚。
// appleRoots 是包级变量，测试期换掉再还原即可 —— 换 pool 而不是给 VerifyAppleJWS
// 加一个「测试用根证书」参数：那个参数会在生产代码里一直存在，且总有一天会被传值。
func fakeAppleChain(t *testing.T) (leaf *testCert, x5c [][]byte) {
	t.Helper()
	nb := time.Now().Add(-time.Hour)
	na := time.Now().Add(24 * time.Hour)
	root := mkCert(t, "Fake Apple Root", true, nil, nb, na)
	inter := mkCert(t, "Fake Apple WWDR", true, root, nb, na)
	leaf = mkCert(t, "Fake Apple Leaf", false, inter, nb, na)

	pool := x509.NewCertPool()
	pool.AddCert(root.cert)
	prev := appleRoots
	appleRoots = pool
	t.Cleanup(func() { appleRoots = prev })

	return leaf, [][]byte{leaf.der, inter.der, root.der}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signAppleJWS(t *testing.T, key *ecdsa.PrivateKey, x5c [][]byte, alg string, payload any) string {
	t.Helper()
	certs := make([]string, 0, len(x5c))
	for _, der := range x5c {
		// x5c 是标准 base64，不是 base64url
		certs = append(certs, base64.StdEncoding.EncodeToString(der))
	}
	hdr, err := json.Marshal(map[string]any{"alg": alg, "x5c": certs})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	input := b64u(hdr) + "." + b64u(body)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64u(sig)
}

// ─────────────────────────── Apple 验签 ───────────────────────────

func TestVerifyAppleJWS(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	tok := signAppleJWS(t, leaf.key, x5c, "ES256", map[string]any{"hello": "world"})

	payload, err := VerifyAppleJWS(tok, time.Now())
	if err != nil {
		t.Fatalf("正常签名应通过：%v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(payload, &got); err != nil || got["hello"] != "world" {
		t.Fatalf("payload 不对：%s (%v)", payload, err)
	}
}

// 链上任何一节不是我们信任的根签的，就一律不认。这是整套收单里唯一能区分
// 「苹果发的」和「谁都能造的」的判据。
func TestVerifyAppleJWSRejectsForeignChain(t *testing.T) {
	_, _ = fakeAppleChain(t) // 装好我们信任的那条链
	nb, na := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	otherRoot := mkCert(t, "Attacker Root", true, nil, nb, na)
	otherLeaf := mkCert(t, "Attacker Leaf", false, otherRoot, nb, na)
	tok := signAppleJWS(t, otherLeaf.key,
		[][]byte{otherLeaf.der, otherRoot.der}, "ES256", map[string]any{"hello": "evil"})

	if _, err := VerifyAppleJWS(tok, time.Now()); err == nil {
		t.Fatal("自签链必须被拒")
	}
}

func TestVerifyAppleJWSRejectsTamperedPayload(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	tok := signAppleJWS(t, leaf.key, x5c, "ES256", map[string]any{"productId": "squyrrl.plan.basic.monthly"})
	parts := strings.Split(tok, ".")
	parts[1] = b64u([]byte(`{"productId":"squyrrl.plan.maximum.yearly"}`))
	if _, err := VerifyAppleJWS(strings.Join(parts, "."), time.Now()); err == nil {
		t.Fatal("改过 payload 的 token 必须被拒")
	}
}

// alg 由攻击者写在头部里。按头部选算法正是 JWT 的经典漏洞家族
// （alg:none 免签；RS256→HS256 把公开的公钥当 HMAC 密钥）。
func TestVerifyAppleJWSRejectsAlgSwap(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	for _, alg := range []string{"none", "HS256", "ES384", ""} {
		tok := signAppleJWS(t, leaf.key, x5c, alg, map[string]any{"a": 1})
		if _, err := VerifyAppleJWS(tok, time.Now()); err == nil {
			t.Errorf("alg=%q 必须被拒", alg)
		}
	}
}

func TestVerifyAppleJWSRejectsMalformed(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	good := signAppleJWS(t, leaf.key, x5c, "ES256", map[string]any{"a": 1})
	parts := strings.Split(good, ".")

	noX5c, _ := json.Marshal(map[string]any{"alg": "ES256"})
	cases := map[string]string{
		"段数不对":           parts[0] + "." + parts[1],
		"头部不是 JSON":      b64u([]byte("not json")) + "." + parts[1] + "." + parts[2],
		"没有 x5c":         b64u(noX5c) + "." + parts[1] + "." + parts[2],
		"签名不是 base64url": parts[0] + "." + parts[1] + ".!!!!",
		"空串":             "",
	}
	for name, tok := range cases {
		if _, err := VerifyAppleJWS(tok, time.Now()); err == nil {
			t.Errorf("%s：必须被拒", name)
		}
	}
}

// 证书链有效期是有意按 now 校验的：过期的叶证书说明这条链早已不该再签东西。
func TestVerifyAppleJWSRejectsExpiredChain(t *testing.T) {
	leaf, x5c := fakeAppleChain(t)
	tok := signAppleJWS(t, leaf.key, x5c, "ES256", map[string]any{"a": 1})
	if _, err := VerifyAppleJWS(tok, time.Now().Add(48*time.Hour)); err == nil {
		t.Fatal("证书过期后必须被拒")
	}
}

// JWS 的 ECDSA 签名是定长 R‖S，不是 DER。这条用例把那个坑钉住：
// 一旦有人把 verifyES256 改成 ecdsa.VerifyASN1，它会变红而不是静默全拒。
func TestVerifyES256RejectsDERSignature(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("a.b")
	sum := sha256.Sum256(input)
	der, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if verifyES256(&key.PublicKey, input, der) {
		t.Fatal("DER 形态的签名不该被 JWS 验签接受")
	}
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	if !verifyES256(&key.PublicKey, input, raw) {
		t.Fatal("R‖S 形态的签名应通过")
	}
}

// ─────────────────────────── Google OIDC ───────────────────────────

type staticKeys struct {
	kid string
	key *rsa.PublicKey
}

func (s staticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if kid != s.kid {
		return nil, ErrBadSignature
	}
	return s.key, nil
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims any) string {
	t.Helper()
	hdr, err := json.Marshal(map[string]any{"alg": alg, "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := b64u(hdr) + "." + b64u(body)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64u(sig)
}

const pubsubAud = "https://api.squyrrl.com/webhooks/google"

func googleFixture(t *testing.T, email string) (*rsa.PrivateKey, *GoogleOIDCVerifier) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, NewGoogleOIDCVerifier(pubsubAud, email,
		staticKeys{kid: "k1", key: &key.PublicKey})
}

func goodClaims() map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            pubsubAud,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"email":          "rtdn@squyrrl.iam.gserviceaccount.com",
		"email_verified": true,
	}
}

func TestGoogleOIDCAcceptsGenuineToken(t *testing.T) {
	key, v := googleFixture(t, "rtdn@squyrrl.iam.gserviceaccount.com")
	tok := signRS256(t, key, "k1", "RS256", goodClaims())
	if err := v.Verify(context.Background(), "Bearer "+tok); err != nil {
		t.Fatalf("正常 token 应通过：%v", err)
	}
}

// 这条是本轮修掉的漏洞本身：旧实现只解 payload 比对 aud，于是一段**完全没有
// 签名**的 base64 就能通过 —— /webhooks/google 挂在公网、没有 Bearer 中间件。
func TestGoogleOIDCRejectsUnsignedToken(t *testing.T) {
	_, v := googleFixture(t, "")
	hdr, _ := json.Marshal(map[string]any{"alg": "none", "kid": "k1"})
	body, _ := json.Marshal(goodClaims())
	forged := b64u(hdr) + "." + b64u(body) + "."
	if err := v.Verify(context.Background(), "Bearer "+forged); err == nil {
		t.Fatal("未签名的 token 必须被拒")
	}
}

func TestGoogleOIDCRejectsWrongKeyAndTamper(t *testing.T) {
	key, v := googleFixture(t, "")

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(context.Background(),
		"Bearer "+signRS256(t, other, "k1", "RS256", goodClaims())); err == nil {
		t.Error("别的密钥签的 token 必须被拒")
	}
	if err := v.Verify(context.Background(),
		"Bearer "+signRS256(t, key, "k-unknown", "RS256", goodClaims())); err == nil {
		t.Error("未知 kid 必须被拒")
	}

	tok := signRS256(t, key, "k1", "RS256", goodClaims())
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + b64u([]byte(`{"aud":"`+pubsubAud+`"}`)) + "." + parts[2]
	if err := v.Verify(context.Background(), "Bearer "+tampered); err == nil {
		t.Error("改过 claims 的 token 必须被拒")
	}
}

func TestGoogleOIDCRejectsBadClaims(t *testing.T) {
	key, v := googleFixture(t, "rtdn@squyrrl.iam.gserviceaccount.com")
	cases := map[string]func(map[string]any){
		"aud 不匹配":      func(c map[string]any) { c["aud"] = "https://evil.example/hook" },
		"签发方不是 Google": func(c map[string]any) { c["iss"] = "https://evil.example" },
		"已过期":          func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"没有 exp":       func(c map[string]any) { delete(c, "exp") },
		// 签名是真的，但签的人不是我们那个 Pub/Sub 服务账号 —— Google 给所有
		// OIDC token 用的是同一套密钥，不比对签发对象等于谁都能进来。
		"服务账号不对": func(c map[string]any) { c["email"] = "anyone@gmail.com" },
		"邮箱未经验证": func(c map[string]any) { c["email_verified"] = false },
	}
	for name, mut := range cases {
		claims := goodClaims()
		mut(claims)
		if err := v.Verify(context.Background(), "Bearer "+signRS256(t, key, "k1", "RS256", claims)); err == nil {
			t.Errorf("%s：必须被拒", name)
		}
	}
}

// 没配 audience 就是没启用。fail-closed —— 漏配的表现必须是「webhook 全拒」，
// 而不是「webhook 全收」。
func TestGoogleOIDCDisabledWithoutAud(t *testing.T) {
	key, _ := googleFixture(t, "")
	v := NewGoogleOIDCVerifier("", "", staticKeys{kid: "k1", key: &key.PublicKey})
	if v.Enabled() {
		t.Fatal("没配 aud 不该算启用")
	}
	if err := v.Verify(context.Background(), "Bearer "+signRS256(t, key, "k1", "RS256", goodClaims())); err == nil {
		t.Fatal("未启用时必须一律拒绝")
	}
}

func TestParseJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := b64u(key.N.Bytes())
	e := b64u(big.NewInt(int64(key.E)).Bytes())
	body := `{"keys":[
		{"kty":"RSA","alg":"RS256","kid":"k1","n":"` + n + `","e":"` + e + `"},
		{"kty":"EC","alg":"ES256","kid":"skip-me","n":"` + n + `","e":"` + e + `"}
	]}`
	keys, err := parseJWKS([]byte(body))
	if err != nil {
		t.Fatalf("应解析成功：%v", err)
	}
	if len(keys) != 1 || keys["k1"] == nil {
		t.Fatalf("只该收下 RS256 的那把，实际 %d 把", len(keys))
	}
	if keys["k1"].N.Cmp(key.N) != 0 || keys["k1"].E != key.E {
		t.Error("公钥解出来不对")
	}
	// 一份解不出任何可用密钥的回包是故障，不是「空密钥集」—— 后者会让
	// 缓存被一份空表覆盖，随后所有 webhook 静默失败一小时。
	if _, err := parseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Error("空密钥集应报错")
	}
}
