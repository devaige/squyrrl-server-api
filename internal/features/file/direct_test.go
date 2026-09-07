package file

import (
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

// 直传必须两项配齐才算可用：只有令牌密钥而没有边缘地址时若判为「可用」，
// IssueIntent 会签出一个 upload_url 为空的回包，把「往哪传」的决定权推给客户端。
func TestDirectUploadEnabledRequiresBoth(t *testing.T) {
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
		if got := NewService(nil, nil, c.base, c.secret).DirectUploadEnabled(); got != c.want {
			t.Errorf("base=%q secret=%q ⇒ %v, want %v", c.base, c.secret, got, c.want)
		}
	}
}
