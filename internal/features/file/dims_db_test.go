package file

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/infra/db"
)

// 直传的尺寸在签发时落进 upload_intents、收尾时搬进 files，中间那一段是 SQL，
// 纯 Go 单测碰不到。CI 没有 Postgres，未设变量即跳过：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/file/ -run IntentDims -v
func TestIntentDimsRoundTrip(t *testing.T) {
	url := os.Getenv("SQUYRRL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("未设 SQUYRRL_TEST_DATABASE_URL，跳过需要真库的测试")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("测试库迁移失败: %v", err)
	}

	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"dims-"+uuid.NewString()+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	hash := func() []byte {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return b
	}

	repo := NewRepo(pool)
	cases := []struct {
		name            string
		dims, thumbDims Dims
		thumbHash       []byte
	}{
		{"带缩略图", Dims{4000, 3000}, Dims{512, 384}, hash()},
		{"全部未知", Dims{}, Dims{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cipher := hash()
			var thumbSize int64
			if c.thumbHash != nil {
				thumbSize = 1024
			}
			it, err := repo.InsertIntent(ctx, &UploadIntent{
				UserID: userID, PlainHash: hash(), CipherHash: cipher, SizeBytes: 4096,
				Mime: "image/jpeg", StorageKey: StorageKeyFor(cipher),
				PartSize: PartSize, PartCount: 1, ExpiresAt: time.Now().Add(time.Hour),
				ThumbCipherHash: c.thumbHash, ThumbSizeBytes: thumbSize,
				Dims: c.dims, ThumbDims: c.thumbDims,
			})
			if err != nil {
				t.Fatalf("写意图失败: %v", err)
			}
			got, err := repo.GetIntentForUser(ctx, it.ID, userID)
			if err != nil {
				t.Fatalf("读意图失败: %v", err)
			}
			if got.Dims != c.dims || got.ThumbDims != c.thumbDims {
				t.Fatalf("尺寸往返不一致: %+v / %+v", got.Dims, got.ThumbDims)
			}
		})
	}
}
