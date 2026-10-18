package file

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/infra/db"
)

// 续期的互斥条件写在 SQL 里（status='pending'），纯 Go 单测碰不到。CI 没有 Postgres，未设变量即跳过：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/file/ -run ExtendIntent -v
func TestExtendIntentOnlyWhilePending(t *testing.T) {
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
		"renew-"+uuid.NewString()+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	hash := func() []byte {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return b
	}
	repo := NewRepo(pool)
	insert := func(expires time.Time) *UploadIntent {
		cipher := hash()
		it, err := repo.InsertIntent(ctx, &UploadIntent{
			UserID: userID, PlainHash: hash(), CipherHash: cipher, SizeBytes: 4096,
			Mime: "video/mp4", StorageKey: StorageKeyFor(cipher),
			PartSize: PartSize, PartCount: 1, ExpiresAt: expires,
		})
		if err != nil {
			t.Fatalf("写意图失败: %v", err)
		}
		return it
	}

	// 已过期、sweeper 还没领走：照样能续，续完不再是 sweeper 的目标
	expired := insert(time.Now().Add(-time.Minute))
	next := time.Now().Add(intentTTL).Truncate(time.Second)
	if err := repo.ExtendIntent(ctx, expired.ID, next); err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	got, err := repo.GetIntentForUser(ctx, expired.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(next) {
		t.Fatalf("过期时刻没更新：%v", got.ExpiresAt)
	}

	// 已收尾的不能续：客户端据此丢掉断点从头传
	done := insert(time.Now().Add(time.Hour))
	if err := repo.MarkIntentStatus(ctx, done.ID, "committed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ExtendIntent(ctx, done.ID, next); !errors.Is(err, ErrIntentState) {
		t.Fatalf("已收尾的意图应拒绝续期，拿到 %v", err)
	}
}
