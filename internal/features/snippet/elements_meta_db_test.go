package snippet

import (
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/infra/db"
)

// 列表卡片靠 elements 里带出的文件元数据排版（2026-10-14），那段是拼在 snippetCols
// 里的 jsonb，纯 Go 单测碰不到。CI 没有 Postgres，未设变量即跳过：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/snippet/ -run ElementsCarryFileMeta -v
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SQUYRRL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("未设 SQUYRRL_TEST_DATABASE_URL，跳过需要真库的测试")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		pool.Close()
		t.Fatalf("测试库迁移失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randHash(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestElementsCarryFileMeta(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"meta-"+uuid.NewString()+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	files := file.NewRepo(pool)
	insert := func(mime string, size int64, dims file.Dims) *file.File {
		cipher := randHash(t)
		f, err := files.Insert(ctx, randHash(t), cipher, size, mime, "squyrrl",
			file.StorageKeyFor(cipher), dims)
		if err != nil {
			t.Fatalf("建文件失败: %v", err)
		}
		return f
	}
	photo := insert("image/jpeg", 2_000_000, file.Dims{Width: 4000, Height: 3000})
	legacy := insert("application/pdf", 300_000, file.Dims{})

	thumbDims := file.Dims{Width: 512, Height: 384}
	if ok, err := files.SetThumbnail(ctx, photo.ID, file.ThumbnailKeyFor(photo.StorageKey), thumbDims); err != nil || !ok {
		t.Fatalf("挂缩略图失败: ok=%v err=%v", ok, err)
	}
	// 一行只有一个槽位：第二次必须是空操作，不能覆盖先到的那份
	if ok, err := files.SetThumbnail(ctx, photo.ID, "thumb/other.jpg", file.Dims{Width: 1, Height: 1}); err != nil || ok {
		t.Fatalf("已有缩略图时不该再写入: ok=%v err=%v", ok, err)
	}

	s, err := NewRepo(pool).Create(ctx, userID, &CreateInput{
		Type: "normal", SourceType: "device",
		Elements: []ElementInput{
			{FileID: photo.ID, OrderIdx: 0},
			{FileID: legacy.ID, OrderIdx: 1},
		},
	})
	if err != nil {
		t.Fatalf("建碎片失败: %v", err)
	}
	if len(s.Elements) != 2 {
		t.Fatalf("elements 数量 = %d", len(s.Elements))
	}

	p, l := s.Elements[0], s.Elements[1]
	if p.Mime != "image/jpeg" || p.SizeBytes != 2_000_000 || !p.HasThumbnail ||
		p.Width != 4000 || p.Height != 3000 || p.ThumbWidth != 512 || p.ThumbHeight != 384 {
		t.Fatalf("图片附件的元数据不对: %+v", p)
	}
	if l.Mime != "application/pdf" || l.SizeBytes != 300_000 || l.HasThumbnail ||
		l.Width != 0 || l.ThumbWidth != 0 {
		t.Fatalf("未知尺寸的附件应全部落成零值: %+v", l)
	}
}
