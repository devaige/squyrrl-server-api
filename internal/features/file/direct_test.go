package file

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPartCountFor(t *testing.T) {
	cases := []struct {
		size int64
		want int
	}{
		{1, 1},
		{PartSize - 1, 1},
		{PartSize, 1},     // 恰好一片：走单片模式，省两次 Class A
		{PartSize + 1, 2}, // 溢出一个字节就必须 multipart
		{PartSize * 3, 3},
		{PartSize*3 + 1, 4},
		{100 * 1024 * 1024, 13}, // 上限文件：13 片，每片远低于边缘 100MB 请求体限制
	}
	for _, c := range cases {
		if got := PartCountFor(c.size); got != c.want {
			t.Errorf("PartCountFor(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

func TestUploadToken_RoundTrip(t *testing.T) {
	const secret = "test-secret"
	in := UploadClaims{
		IntentID:   "11111111-1111-1111-1111-111111111111",
		Key:        "deadbeef",
		UploadID:   "r2-upload-id",
		SizeBytes:  1234,
		PartSize:   PartSize,
		PartCount:  2,
		CipherHash: "abc123",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	}
	tok, err := SignUploadToken(secret, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := VerifyUploadToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if out.Key != in.Key || out.UploadID != in.UploadID || out.PartCount != in.PartCount {
		t.Fatalf("claims 往返不一致：%+v vs %+v", out, in)
	}
}

func TestUploadToken_Rejects(t *testing.T) {
	const secret = "test-secret"
	valid, _ := SignUploadToken(secret, UploadClaims{
		Key: "k", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	if _, err := VerifyUploadToken("wrong-secret", valid); err != ErrBadToken {
		t.Errorf("换密钥应判 ErrBadToken，得到 %v", err)
	}
	if _, err := VerifyUploadToken(secret, "no-dot-here"); err != ErrBadToken {
		t.Errorf("缺分隔符应判 ErrBadToken，得到 %v", err)
	}

	// 篡改 payload 而保留原签名：这正是「key 由服务端签死」要挡住的攻击
	payload, mac, _ := strings.Cut(valid, ".")
	tampered, _ := SignUploadToken(secret, UploadClaims{
		Key: "someone-elses-key", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	tamperedPayload, _, _ := strings.Cut(tampered, ".")
	if _, err := VerifyUploadToken(secret, tamperedPayload+"."+mac); err != ErrBadToken {
		t.Errorf("换 payload 保留旧签名应判 ErrBadToken，得到 %v", err)
	}
	_ = payload

	expired, _ := SignUploadToken(secret, UploadClaims{
		Key: "k", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	if _, err := VerifyUploadToken(secret, expired); err != ErrTokenExpired {
		t.Errorf("过期应判 ErrTokenExpired，得到 %v", err)
	}
}

func TestOrderParts(t *testing.T) {
	// 乱序交回应被排好序
	got, err := orderParts([]CommitPart{
		{PartNumber: 3, ETag: "c"},
		{PartNumber: 1, ETag: "a"},
		{PartNumber: 2, ETag: "b"},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].ETag != want || got[i].PartNumber != i+1 {
			t.Fatalf("排序错误：%+v", got)
		}
	}

	bad := []struct {
		name  string
		parts []CommitPart
		want  int
	}{
		{"数量不足", []CommitPart{{PartNumber: 1, ETag: "a"}}, 2},
		{"重号", []CommitPart{{PartNumber: 1, ETag: "a"}, {PartNumber: 1, ETag: "b"}}, 2},
		{"越界", []CommitPart{{PartNumber: 1, ETag: "a"}, {PartNumber: 3, ETag: "c"}}, 2},
		{"零号", []CommitPart{{PartNumber: 0, ETag: "a"}, {PartNumber: 1, ETag: "b"}}, 2},
	}
	for _, c := range bad {
		if _, err := orderParts(c.parts, c.want); err != ErrPartsMismatch {
			t.Errorf("%s：应判 ErrPartsMismatch，得到 %v", c.name, err)
		}
	}
}

// 边缘必须两项配齐才算可用：只有令牌密钥而没有边缘地址时若判为「可用」，
// IssueIntent 会签出一个 upload_url 为空的回包，把「往哪传」的决定权推给客户端；
// IssueTicket 同理会签出一个 "/v1/blob?t=..." 这样没有 host 的下载 URL。
func TestEdgeEnabledRequiresBoth(t *testing.T) {
	cases := []struct {
		base, secret string
		want         bool
	}{
		{"", "", false},
		{"https://files.squyrrl.com", "", false},
		{"", "secret", false},
		{"https://files.squyrrl.com", "secret", true},
	}
	for _, c := range cases {
		if got := NewService(nil, nil, nil, c.base, c.secret).EdgeEnabled(); got != c.want {
			t.Errorf("base=%q secret=%q ⇒ %v, want %v", c.base, c.secret, got, c.want)
		}
	}
}

// 上传令牌与下载令牌共用一个 secret，但签名输入里混了用途，所以互不通用。
//
// 这条不是形式主义：下载令牌没有 sz/ch 字段，若能当上传令牌用，边缘解出的
// claims 会是「期望 0 字节、无需校验哈希」，于是把那个内容寻址的 key 覆盖成空对象 ——
// 而该 key 的字节是全体引用者共享的。
func TestTokenPurposesAreNotInterchangeable(t *testing.T) {
	const secret = "s3cr3t"
	exp := time.Now().Add(time.Minute).Unix()

	up, err := SignUploadToken(secret, UploadClaims{Key: "k", SizeBytes: 1, ExpiresAt: exp})
	if err != nil {
		t.Fatalf("签上传令牌失败：%v", err)
	}
	down, err := SignDownloadToken(secret, DownloadClaims{Key: "k", Mime: "image/png", ExpiresAt: exp})
	if err != nil {
		t.Fatalf("签下载令牌失败：%v", err)
	}

	if _, err := VerifyUploadToken(secret, down); err != ErrBadToken {
		t.Errorf("下载令牌被当成上传令牌接受了：%v", err)
	}
	if _, err := VerifyDownloadToken(secret, up); err != ErrBadToken {
		t.Errorf("上传令牌被当成下载令牌接受了：%v", err)
	}
}

func TestDownloadTokenRoundTrip(t *testing.T) {
	const secret = "s3cr3t"
	in := DownloadClaims{Key: "beef", Mime: "image/jpeg", ExpiresAt: time.Now().Add(time.Minute).Unix()}

	tok, err := SignDownloadToken(secret, in)
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	out, err := VerifyDownloadToken(secret, tok)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if *out != in {
		t.Errorf("往返后不一致：%+v != %+v", *out, in)
	}

	if _, err := VerifyDownloadToken("wrong-secret", tok); err != ErrBadToken {
		t.Errorf("换密钥应判 ErrBadToken，得到 %v", err)
	}
	expired, _ := SignDownloadToken(secret, DownloadClaims{Key: "k", ExpiresAt: time.Now().Add(-time.Second).Unix()})
	if _, err := VerifyDownloadToken(secret, expired); err != ErrTokenExpired {
		t.Errorf("过期应判 ErrTokenExpired，得到 %v", err)
	}
}

func TestStorageKeyFor_PrefixedAndContentAddressed(t *testing.T) {
	h := []byte{0xde, 0xad, 0xbe, 0xef}
	if got, want := StorageKeyFor(h), "blob/deadbeef"; got != want {
		t.Fatalf("StorageKeyFor = %q, want %q", got, want)
	}
}

// 缩略图 key 必须与本体 key **平级**，不能嵌套成 thumb/blob/…。
// 两类对象靠前缀区分保留策略，前缀一旦嵌套，按 blob/ 配的生命周期规则会把缩略图
// 一并扫进去。这正是 ThumbnailKeyFor 取 path.Base 而不是直接拼接的原因。
func TestThumbnailKeyFor_NotNestedUnderBlobPrefix(t *testing.T) {
	got := ThumbnailKeyFor(StorageKeyFor([]byte{0xde, 0xad, 0xbe, 0xef}))
	if want := "thumb/deadbeef.jpg"; got != want {
		t.Fatalf("ThumbnailKeyFor = %q, want %q", got, want)
	}
	if strings.Contains(got, blobPrefix) {
		t.Fatalf("缩略图 key %q 嵌套了 blob/ 前缀", got)
	}
}

// 不带前缀的历史 key 也要能推出正确的缩略图 key（path.Base 顺带兼容）。
func TestThumbnailKeyFor_LegacyUnprefixedKey(t *testing.T) {
	if got, want := ThumbnailKeyFor("deadbeef"), "thumb/deadbeef.jpg"; got != want {
		t.Fatalf("ThumbnailKeyFor = %q, want %q", got, want)
	}
}

// =============================================================================
// 客户端缩略图（成本审计 #7）
// =============================================================================

func TestValidateThumb(t *testing.T) {
	hash := make([]byte, 32)
	const blob = 4 * 1024 * 1024

	cases := []struct {
		name      string
		mime      string
		size      int64
		thumbSize int64
		thumbHash []byte
		want      error
	}{
		{"没声明就是合法的", "application/pdf", blob, 0, nil, nil},
		// 没声明时连 mime 都不该看：不带缩略图是所有文件类型的默认状态。
		{"没声明时不看 mime", "video/mp4", blob, 0, nil, nil},
		{"正常图片", "image/jpeg", blob, 20 * 1024, hash, nil},
		{"非图片不许带", "application/pdf", blob, 20 * 1024, hash, ErrThumbNotImage},
		{"超过 64 KiB 上限", "image/png", blob, MaxThumbBytes + 1, hash, ErrThumbTooLarge},
		{"恰好 64 KiB 可以", "image/png", blob, MaxThumbBytes, hash, nil},
		// 这条是把「不计配额的字节」框死的不变式，不是手滑校验：
		// 允许缩略图 ≥ 本体，一个 1 字节的本体就能拖 64 KiB 未计费字节进 R2。
		{"不得大于等于本体", "image/jpeg", 1024, 1024, hash, ErrThumbTooLarge},
		{"比本体小一个字节也行", "image/jpeg", 1024, 1023, hash, nil},
		{"声明了却给 0 大小", "image/jpeg", blob, 0, hash, ErrThumbTooLarge},
		{"负数大小", "image/jpeg", blob, -1, hash, ErrThumbTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateThumb(c.mime, c.size, c.thumbSize, c.thumbHash); !errors.Is(got, c.want) {
				t.Fatalf("validateThumb = %v, want %v", got, c.want)
			}
		})
	}
}

// 缩略图票必须是「单片、另一个 key」。UploadID 非空会让边缘把它路由到 multipart
// 分支并跳过整体哈希校验；key 相同则会让缩略图覆盖本体 —— 而 key 是全局去重的，
// 一次覆盖波及所有引用者。
func TestThumbTokenIsSinglePartAndTargetsThumbKey(t *testing.T) {
	const secret = "test-secret"
	blobKey := StorageKeyFor(make([]byte, 32))
	exp := time.Now().Add(time.Hour).Unix()

	tok, err := SignUploadToken(secret, UploadClaims{
		IntentID:   "11111111-1111-1111-1111-111111111111",
		Key:        ThumbnailKeyFor(blobKey),
		SizeBytes:  20 * 1024,
		PartSize:   20 * 1024,
		PartCount:  1,
		CipherHash: strings.Repeat("ab", 32),
		ExpiresAt:  exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyUploadToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UploadID != "" {
		t.Fatalf("缩略图票不该带 uploadId，拿到 %q", claims.UploadID)
	}
	if claims.Key == blobKey {
		t.Fatal("缩略图票指向了本体的 key —— 会覆盖全局去重的那份字节")
	}
	if !strings.HasPrefix(claims.Key, thumbPrefix) {
		t.Fatalf("缩略图 key 前缀不对：%q", claims.Key)
	}
	if claims.CipherHash == "" {
		t.Fatal("缩略图票必须带整体哈希，单片模式靠它做完整性校验")
	}
}
