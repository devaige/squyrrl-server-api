package users

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/db"
)

// 搜索语义落在 SQL 上（citext 列上的 ILIKE、通配符转义、软删除过滤），纯 Go 单测碰不到，
// 所以这组测试打真库。未设变量即跳过，原因同 auth 包的 refresh_slide_test.go。
// 使用显式提供的一次性测试库运行，不能指向正式库：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/users/ -run Search -v
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

// seedUser 直接写 users 表；tag 让同一测试库里多次运行互不干扰。
func seedUser(t *testing.T, pool *pgxpool.Pool, email string, deleted bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, deleted_at)
		VALUES ($1, CASE WHEN $2 THEN now() END)
		RETURNING id`, email, deleted).Scan(&id)
	if err != nil {
		t.Fatalf("建用户 %s 失败: %v", email, err)
	}
	return id
}

func ids(items []Summary) map[uuid.UUID]bool {
	m := make(map[uuid.UUID]bool, len(items))
	for _, s := range items {
		m[s.ID] = true
	}
	return m
}

func TestSearch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE email LIKE '%' || $1 || '%'`, tag)
	})

	underscore := seedUser(t, pool, "john_doe-"+tag+"@example.test", false)
	lookalike := seedUser(t, pool, "johnxdoe-"+tag+"@example.test", false)
	alice := seedUser(t, pool, "Alice-"+tag+"@Example.test", false)
	gone := seedUser(t, pool, "gone-"+tag+"@example.test", true)

	walletSvc := wallet.NewService(wallet.NewRepo(pool))
	if _, err := walletSvc.Grant(ctx, alice, 1234, "test", ""); err != nil {
		t.Fatalf("发币失败: %v", err)
	}
	subSvc := subscriptions.NewService(subscriptions.NewRepo(pool), walletSvc, nil)
	if err := subSvc.SetAdminPlan(ctx, underscore, "premium"); err != nil {
		t.Fatalf("设置手动档位失败: %v", err)
	}
	if _, err := subSvc.GrantAdminStorage(ctx, underscore, "s40"); err != nil {
		t.Fatalf("授予存储失败: %v", err)
	}
	svc := NewService(NewRepo(pool), walletSvc, quota.NewService(pool, walletSvc), subSvc)

	search := func(q string, limit int) []Summary {
		t.Helper()
		items, err := svc.Search(ctx, q, limit)
		if err != nil {
			t.Fatalf("Search(%q) 失败: %v", q, err)
		}
		return items
	}

	t.Run("邮箱子串命中且排除软删除", func(t *testing.T) {
		got := ids(search(tag, MaxLimit))
		if len(got) != 3 || !got[underscore] || !got[lookalike] || !got[alice] {
			t.Fatalf("期望命中 3 个未删除用户，实际 %v", got)
		}
		if got[gone] {
			t.Fatal("软删除的用户不应出现")
		}
	})

	t.Run("下划线按字面匹配", func(t *testing.T) {
		got := ids(search("john_doe-"+tag, MaxLimit))
		if len(got) != 1 || !got[underscore] {
			t.Fatalf("期望只命中 john_doe，实际 %v", got)
		}
	})

	t.Run("邮箱不区分大小写", func(t *testing.T) {
		got := ids(search("ALICE-"+tag+"@EXAMPLE", MaxLimit))
		if len(got) != 1 || !got[alice] {
			t.Fatalf("期望命中 Alice，实际 %v", got)
		}
	})

	t.Run("完整 UUID 精确匹配", func(t *testing.T) {
		items := search(alice.String(), MaxLimit)
		if len(items) != 1 || items[0].ID != alice {
			t.Fatalf("期望只命中 Alice，实际 %v", ids(items))
		}
		// 钱包字段要与 /me/wallet 同源：无订阅即 free/none，余额是流水之和。
		s := items[0]
		if s.Plan != "free" || s.PlanStatus != wallet.PlanStatusNone || s.CreditsBalance != 1234 {
			t.Fatalf("钱包字段不对: plan=%s status=%s balance=%d", s.Plan, s.PlanStatus, s.CreditsBalance)
		}
		if s.Storage.QuotaBytes != 0 || len(s.Subscriptions) != 0 {
			t.Fatalf("无订阅用户不该有存储或订阅行: %+v %+v", s.Storage, s.Subscriptions)
		}
	})

	t.Run("档位、存储与订阅明细同源", func(t *testing.T) {
		items := search(underscore.String(), MaxLimit)
		if len(items) != 1 {
			t.Fatalf("期望命中 1 条，实际 %d 条", len(items))
		}
		s := items[0]
		if s.Plan != "premium" || s.Storage.QuotaBytes != 40<<30 {
			t.Fatalf("plan=%s quota=%d，期望 premium / 40 GiB", s.Plan, s.Storage.QuotaBytes)
		}
		if len(s.Subscriptions) != 2 || s.Subscriptions[0].Kind != "plan" ||
			s.Subscriptions[1].Tier != "s40" || s.Subscriptions[1].Provider != subscriptions.ProviderAdmin {
			t.Fatalf("订阅明细不对: %+v", s.Subscriptions)
		}
	})

	t.Run("软删除用户按 UUID 也查不到", func(t *testing.T) {
		if items := search(gone.String(), MaxLimit); len(items) != 0 {
			t.Fatalf("期望为空，实际 %v", ids(items))
		}
	})

	t.Run("limit 生效", func(t *testing.T) {
		if items := search(tag, 1); len(items) != 1 {
			t.Fatalf("期望 1 条，实际 %d 条", len(items))
		}
	})
}
