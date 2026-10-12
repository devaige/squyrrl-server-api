package subscriptions

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/db"
)

func TestStillLive(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name      string
		rec       Record
		want      bool
		wantGrace bool
	}{
		{"active 不看到期日", Record{Status: "active", PeriodEnd: now.Add(-30 * day)}, true, false},
		{"月付 past_due 宽限期内", Record{Status: "past_due", BillingPeriod: "monthly", PeriodEnd: now.Add(-6 * day)}, true, true},
		{"月付 past_due 宽限期已过", Record{Status: "past_due", BillingPeriod: "monthly", PeriodEnd: now.Add(-8 * day)}, false, false},
		{"年付 past_due 宽限期更长", Record{Status: "past_due", BillingPeriod: "yearly", PeriodEnd: now.Add(-8 * day)}, true, true},
		{"canceled", Record{Status: "canceled", PeriodEnd: now.Add(day)}, false, false},
		{"expired", Record{Status: "expired", PeriodEnd: now.Add(day)}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := c.rec
			if got := stillLive(&rec, now); got != c.want {
				t.Fatalf("stillLive = %v，期望 %v", got, c.want)
			}
			if (rec.GraceUntil != nil) != c.wantGrace {
				t.Fatalf("GraceUntil = %v，期望有值 = %v", rec.GraceUntil, c.wantGrace)
			}
		})
	}
}

// 换档与手动授予的正确性全在 SQL 上（部分唯一索引、列名、IS DISTINCT FROM），
// 纯 Go 单测碰不到，所以下面这组打真库。未设变量即跳过，约定同 users 包的 search_test.go。
// 使用显式提供的一次性测试库运行，不能指向正式库：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/subscriptions/ -v
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

// seedUser 建一个一次性用户，测试结束连同其订阅（ON DELETE CASCADE）一起删掉。
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	email := "sub-" + uuid.NewString() + "@example.test"
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

type fixture struct {
	pool    *pgxpool.Pool
	svc     *Service
	plans   *wallet.Repo
	storage *quota.Service
}

func newFixture(t *testing.T) fixture {
	pool := testPool(t)
	walletSvc := wallet.NewService(wallet.NewRepo(pool))
	return fixture{
		pool:    pool,
		svc:     NewService(NewRepo(pool), walletSvc),
		plans:   wallet.NewRepo(pool),
		storage: quota.NewService(pool, walletSvc),
	}
}

func (f fixture) plan(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	st, err := f.plans.PlanState(context.Background(), userID)
	if err != nil {
		t.Fatalf("PlanState 失败: %v", err)
	}
	return st.Tier
}

func (f fixture) quotaGB(t *testing.T, userID uuid.UUID) int64 {
	t.Helper()
	st, err := f.storage.Storage(context.Background(), userID)
	if err != nil {
		t.Fatalf("Storage 失败: %v", err)
	}
	return st.QuotaBytes >> 30
}

func (f fixture) status(t *testing.T, provider Provider, providerID string) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status FROM subscriptions
		WHERE payment_provider = $1 AND payment_provider_subscription_id = $2`,
		string(provider), providerID).Scan(&s); err != nil {
		t.Fatalf("查 %s/%s 状态失败: %v", provider, providerID, err)
	}
	return s
}

func event(userID uuid.UUID, provider Provider, providerID, kind, tier string, gb *int) *SubscriptionEvent {
	now := time.Now()
	return &SubscriptionEvent{
		Provider:               provider,
		ProviderSubscriptionID: providerID,
		UserID:                 userID,
		Kind:                   kind,
		Tier:                   tier,
		BillingPeriod:          "monthly",
		Status:                 "active",
		PeriodStart:            now,
		PeriodEnd:              now.Add(30 * 24 * time.Hour),
		BonusStorageGB:         gb,
	}
}

func intp(n int) *int { return &n }

func TestApplyReplacesPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 渠道 ID 不同的两条 active plan：旧实现先 INSERT 再让旧行退场，
	// INSERT 当场撞 uq_subscriptions_one_active_plan。
	t.Run("换渠道重新购买", func(t *testing.T) {
		u := seedUser(t, f.pool)
		stripeID, appleID := "sub_"+uuid.NewString(), "txn_"+uuid.NewString()
		if err := f.svc.Apply(ctx, event(u, ProviderStripe, stripeID, "plan", "basic", nil)); err != nil {
			t.Fatalf("Stripe basic 落库失败: %v", err)
		}
		if err := f.svc.Apply(ctx, event(u, ProviderApple, appleID, "plan", "premium", nil)); err != nil {
			t.Fatalf("Apple premium 落库失败: %v", err)
		}
		if got := f.plan(t, u); got != "premium" {
			t.Fatalf("档位 = %s，期望 premium", got)
		}
		if got := f.status(t, ProviderStripe, stripeID); got != "expired" {
			t.Fatalf("被取代的 Stripe 行状态 = %s，期望 expired", got)
		}
	})

	t.Run("同一条订阅续期不会把自己顶掉", func(t *testing.T) {
		u := seedUser(t, f.pool)
		id := "sub_" + uuid.NewString()
		for i := 0; i < 2; i++ {
			if err := f.svc.Apply(ctx, event(u, ProviderStripe, id, "plan", "standard", nil)); err != nil {
				t.Fatalf("第 %d 次落库失败: %v", i+1, err)
			}
		}
		if got := f.status(t, ProviderStripe, id); got != "active" {
			t.Fatalf("续期后状态 = %s，期望 active", got)
		}
	})

	// Play 换档换了 purchaseToken。plan 这一侧旧实现同样死在 INSERT 上。
	t.Run("Play 换档 plan", func(t *testing.T) {
		u := seedUser(t, f.pool)
		oldToken, newToken := "tok_"+uuid.NewString(), "tok_"+uuid.NewString()
		if err := f.svc.Apply(ctx, event(u, ProviderGoogle, oldToken, "plan", "basic", nil)); err != nil {
			t.Fatalf("旧 token 落库失败: %v", err)
		}
		e := event(u, ProviderGoogle, newToken, "plan", "maximum", nil)
		e.SupersedesProviderID = oldToken
		if err := f.svc.Apply(ctx, e); err != nil {
			t.Fatalf("新 token 落库失败: %v", err)
		}
		if got := f.plan(t, u); got != "maximum" {
			t.Fatalf("档位 = %s，期望 maximum", got)
		}
	})

	// storage 没有唯一约束兜底，旧 token 全靠 ExpireByProviderID 退场 ——
	// 旧实现写错了列名，这一步直接报 SQL 错误。
	t.Run("Play 换档 storage", func(t *testing.T) {
		u := seedUser(t, f.pool)
		oldToken, newToken := "tok_"+uuid.NewString(), "tok_"+uuid.NewString()
		if err := f.svc.Apply(ctx, event(u, ProviderGoogle, oldToken, "storage", "s40", intp(40))); err != nil {
			t.Fatalf("旧 token 落库失败: %v", err)
		}
		e := event(u, ProviderGoogle, newToken, "storage", "s80", intp(80))
		e.SupersedesProviderID = oldToken
		if err := f.svc.Apply(ctx, e); err != nil {
			t.Fatalf("新 token 落库失败: %v", err)
		}
		if got := f.status(t, ProviderGoogle, oldToken); got != "expired" {
			t.Fatalf("旧 token 状态 = %s，期望 expired", got)
		}
		if got := f.quotaGB(t, u); got != 80 {
			t.Fatalf("配额 = %d GB，期望 80（旧 token 不该再计入）", got)
		}
	})
}

func TestAdminPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	t.Run("设置、换档、撤回", func(t *testing.T) {
		u := seedUser(t, f.pool)
		for _, tier := range []string{"premium", "basic"} {
			if err := f.svc.SetAdminPlan(ctx, u, tier); err != nil {
				t.Fatalf("设为 %s 失败: %v", tier, err)
			}
			if got := f.plan(t, u); got != tier {
				t.Fatalf("档位 = %s，期望 %s", got, tier)
			}
		}
		recs, err := f.svc.Live(ctx, u)
		if err != nil {
			t.Fatalf("Live 失败: %v", err)
		}
		if len(recs) != 1 || recs[0].Provider != ProviderAdmin || recs[0].Tier != "basic" {
			t.Fatalf("期望只剩一条手动 basic，实际 %+v", recs)
		}

		if err := f.svc.SetAdminPlan(ctx, u, "free"); err != nil {
			t.Fatalf("设为 free 失败: %v", err)
		}
		if got := f.plan(t, u); got != "free" {
			t.Fatalf("档位 = %s，期望 free", got)
		}
	})

	t.Run("有付费订阅时拒绝", func(t *testing.T) {
		u := seedUser(t, f.pool)
		if err := f.svc.Apply(ctx, event(u, ProviderStripe, "sub_"+uuid.NewString(), "plan", "standard", nil)); err != nil {
			t.Fatalf("Stripe 落库失败: %v", err)
		}
		if err := f.svc.SetAdminPlan(ctx, u, "maximum"); !errors.Is(err, ErrPaidPlanActive) {
			t.Fatalf("err = %v，期望 ErrPaidPlanActive", err)
		}
		if got := f.plan(t, u); got != "standard" {
			t.Fatalf("档位 = %s，付费档位不该被动", got)
		}
	})

	t.Run("付费购买取代手动档位", func(t *testing.T) {
		u := seedUser(t, f.pool)
		if err := f.svc.SetAdminPlan(ctx, u, "premium"); err != nil {
			t.Fatalf("设为 premium 失败: %v", err)
		}
		if err := f.svc.Apply(ctx, event(u, ProviderStripe, "sub_"+uuid.NewString(), "plan", "basic", nil)); err != nil {
			t.Fatalf("Stripe 落库失败: %v", err)
		}
		if got := f.plan(t, u); got != "basic" {
			t.Fatalf("档位 = %s，期望以付费的 basic 为准", got)
		}
	})

	t.Run("非法输入", func(t *testing.T) {
		if err := f.svc.SetAdminPlan(ctx, seedUser(t, f.pool), "gold"); !errors.Is(err, ErrUnknownTier) {
			t.Fatalf("未知档位 err = %v", err)
		}
		if err := f.svc.SetAdminPlan(ctx, uuid.New(), "basic"); !errors.Is(err, ErrUnknownUser) {
			t.Fatalf("不存在的用户 err = %v", err)
		}
	})
}

func TestAdminStorage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := seedUser(t, f.pool)

	first, err := f.svc.GrantAdminStorage(ctx, u, "s40")
	if err != nil {
		t.Fatalf("授予 s40 失败: %v", err)
	}
	// 仅原生售卖的小档后台照样能给。
	if _, err := f.svc.GrantAdminStorage(ctx, u, "s10"); err != nil {
		t.Fatalf("授予 s10 失败: %v", err)
	}
	if got := f.quotaGB(t, u); got != 50 {
		t.Fatalf("配额 = %d GB，期望叠加为 50", got)
	}

	if err := f.svc.RevokeAdmin(ctx, u, first); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if got := f.quotaGB(t, u); got != 10 {
		t.Fatalf("撤销后配额 = %d GB，期望 10", got)
	}
	if err := f.svc.RevokeAdmin(ctx, u, first); !errors.Is(err, ErrNotAdminGrant) {
		t.Fatalf("重复撤销 err = %v，期望 ErrNotAdminGrant", err)
	}

	if _, err := f.svc.GrantAdminStorage(ctx, u, "s999"); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("未知存储档位 err = %v", err)
	}

	// 渠道付费的行不归后台撤销。
	paidID := "tok_" + uuid.NewString()
	if err := f.svc.Apply(ctx, event(u, ProviderGoogle, paidID, "storage", "s20", intp(20))); err != nil {
		t.Fatalf("Play 存储落库失败: %v", err)
	}
	var rowID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM subscriptions WHERE payment_provider_subscription_id = $1`,
		paidID).Scan(&rowID); err != nil {
		t.Fatalf("查 Play 行失败: %v", err)
	}
	if err := f.svc.RevokeAdmin(ctx, u, rowID); !errors.Is(err, ErrNotAdminGrant) {
		t.Fatalf("撤销付费行 err = %v，期望 ErrNotAdminGrant", err)
	}
}
