package file

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/infra/db"
)

func TestAdminFilter(t *testing.T) {
	id := uuid.New()
	hash := make([]byte, 32)
	_, _ = rand.Read(hash)
	nonHex := "zz" + hex.EncodeToString(hash)[2:]

	cases := []struct {
		name, q, where string
		arg            any
	}{
		{"空", "  ", "TRUE", nil},
		{"UUID", id.String(), "id = $1", id},
		{"哈希", hex.EncodeToString(hash), "(plain_hash = $1 OR cipher_hash = $1)", hash},
		{"mime 前缀", "image/", "mime ILIKE $1", "image/%"},
		{"通配符转义", `x_y%z\`, "mime ILIKE $1", `x\_y\%z\\%`},
		// 64 个字符但不是 hex：落回 mime 前缀，而不是被当成哈希报错。
		{"非 hex 的 64 字符", nonHex, "mime ILIKE $1", nonHex + "%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			where, args := adminFilter(c.q)
			if where != c.where {
				t.Fatalf("where = %q，期望 %q", where, c.where)
			}
			if c.arg == nil {
				return
			}
			if len(args) != 1 {
				t.Fatalf("参数个数 = %d，期望 1", len(args))
			}
			switch want := c.arg.(type) {
			case []byte:
				if got, ok := args[0].([]byte); !ok || hex.EncodeToString(got) != hex.EncodeToString(want) {
					t.Fatalf("参数 = %v，期望 %x", args[0], want)
				}
			default:
				if args[0] != want {
					t.Fatalf("参数 = %v，期望 %v", args[0], want)
				}
			}
		})
	}
}

// ref_count 由 elements 上的触发器维护，排序与过滤落在 SQL 上，纯 Go 单测碰不到。
// CI 没有 Postgres，未设变量即跳过：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/file/ -run AdminList -v
func TestAdminListRefCountAndSort(t *testing.T) {
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

	var userID, snippetID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"admin-files-"+uuid.NewString()+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO snippets (user_id, type, source_type) VALUES ($1, 'normal', 'device') RETURNING id`,
		userID).Scan(&snippetID); err != nil {
		t.Fatalf("建碎片失败: %v", err)
	}

	repo := NewRepo(pool)
	// 每次跑用一个随机 mime 前缀圈住本轮的文件，测试库里别的行不会混进来。
	mime := "application/x-admin-test-" + uuid.NewString()[:8]
	insert := func() *File {
		plain, cipher := make([]byte, 32), make([]byte, 32)
		_, _ = rand.Read(plain)
		_, _ = rand.Read(cipher)
		f, err := repo.Insert(ctx, plain, cipher, 1024, mime, "test", StorageKeyFor(cipher), Dims{})
		if err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
		return f
	}
	lonely, shared := insert(), insert()
	for range 2 {
		if _, err := pool.Exec(ctx, `INSERT INTO elements (snippet_id, file_id) VALUES ($1, $2)`,
			snippetID, shared.ID); err != nil {
			t.Fatalf("建附件失败: %v", err)
		}
	}

	out, err := repo.AdminList(ctx, AdminListInput{Q: mime, Sort: "refs"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("total=%d items=%d，期望 2/2", out.Total, len(out.Items))
	}
	if out.Items[0].ID != shared.ID || out.Items[0].RefCount != 2 {
		t.Fatalf("按引用排序第一条应是 shared(2)，得到 %s(%d)", out.Items[0].ID, out.Items[0].RefCount)
	}
	if out.Items[1].ID != lonely.ID || out.Items[1].RefCount != 0 {
		t.Fatalf("第二条应是 lonely(0)，得到 %s(%d)", out.Items[1].ID, out.Items[1].RefCount)
	}

	for _, q := range []string{shared.ID.String(), shared.PlainHash, shared.CipherHash} {
		out, err := repo.AdminList(ctx, AdminListInput{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		if out.Total != 1 || out.Items[0].ID != shared.ID {
			t.Fatalf("q=%s 应只命中 shared，得到 total=%d", q, out.Total)
		}
	}

	// 碎片硬删 → elements 级联删除 → 触发器把引用减回 0。
	if _, err := pool.Exec(ctx, `DELETE FROM snippets WHERE id = $1`, snippetID); err != nil {
		t.Fatalf("删碎片失败: %v", err)
	}
	out, err = repo.AdminList(ctx, AdminListInput{Q: shared.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Items[0].RefCount != 0 {
		t.Fatalf("级联删除后 ref_count = %d，期望 0", out.Items[0].RefCount)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM files WHERE id = ANY($1)`, []uuid.UUID{lonely.ID, shared.ID})
}
