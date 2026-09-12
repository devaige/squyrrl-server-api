package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/infra/db"
)

// 滑动过期改的是 SQL 里的一个 GREATEST，纯 Go 单测碰不到它 —— 而这一条要是错了，
// 表现是「用户以为不会被登出，结果还是被登出」或者反过来「到期时间被并发刷新往回拨」，
// 两种都不会有任何编译期或运行期报错。所以这组测试必须打真库。
//
// CI 没有 Postgres（见 .github/workflows/ci.yml），所以**未设变量即跳过**而不是失败：
// 让 CI 因为缺一个它从来就没有的依赖而变红，只会训练大家忽略红灯。
// 本地跑：
//
//	docker compose -f docker-compose.dev.yml up -d postgres
//	SQUYRRL_TEST_DATABASE_URL='postgres://squyrrl:squyrrl_dev@localhost:55432/squyrrl?sslmode=disable' go test ./internal/features/auth/ -run Slide -v
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

// seedSession 造一个「已登录」状态：user + device + session，refresh 到期时间由调用方指定。
// 返回明文 refresh token（Refresh 的入参）与 session id。
func seedSession(t *testing.T, pool *pgxpool.Pool, refreshExp time.Time) (string, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	repo := NewRepo(pool)

	email := "slide-" + uuid.NewString() + "@example.test"
	user, err := repo.UpsertUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	device, _, err := repo.UpsertDevice(ctx, user.ID, "test-device", "linux")
	if err != nil {
		t.Fatalf("建设备失败: %v", err)
	}

	accessTok, err := newOpaqueToken()
	if err != nil {
		t.Fatalf("生成 access token 失败: %v", err)
	}
	refreshTok, err := newOpaqueToken()
	if err != nil {
		t.Fatalf("生成 refresh token 失败: %v", err)
	}
	sessID, err := repo.CreateSession(ctx, user.ID, device.ID,
		hashSHA256(accessTok), hashSHA256(refreshTok),
		time.Now().Add(time.Minute), refreshExp)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	t.Cleanup(func() {
		// users 对 devices / sessions 都是 ON DELETE CASCADE，删根即可。
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user.ID)
	})
	return refreshTok, sessID
}

func refreshExpiryOf(t *testing.T, pool *pgxpool.Pool, sessID uuid.UUID) time.Time {
	t.Helper()
	var exp time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT refresh_expires_at FROM sessions WHERE id = $1`, sessID).Scan(&exp); err != nil {
		t.Fatalf("读 refresh_expires_at 失败: %v", err)
	}
	return exp
}

// 一次刷新应当把到期时间推到「此刻 + refreshTokenTTL」，而不是维持签发时的锚点。
// 这是整批改动的目的：活跃用户不再因为会话到龄而被迫重收一封验证码。
func TestRefreshSlidesExpiryForward(t *testing.T) {
	pool := testPool(t)
	svc := &Service{repo: NewRepo(pool)}

	// 一个「快到期」的会话：还活着（findSession 要求 > now()），但只剩一分钟。
	refreshTok, sessID := seedSession(t, pool, time.Now().Add(time.Minute))

	before := refreshExpiryOf(t, pool, sessID)
	pair, err := svc.Refresh(context.Background(), refreshTok)
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	after := refreshExpiryOf(t, pool, sessID)

	if !after.After(before) {
		t.Fatalf("到期时间没有顺延：before=%s after=%s", before, after)
	}
	want := time.Now().Add(refreshTokenTTL)
	if diff := after.Sub(want); diff > time.Minute || diff < -time.Minute {
		t.Errorf("顺延后的到期时间应约等于 now+%s，实得 %s（偏差 %s）", refreshTokenTTL, after, diff)
	}
	// 返回给客户端的必须是新值。回旧值的话客户端会按一个早已过时的时间安排重登。
	if diff := pair.RefreshExpiresAt.Sub(after); diff > time.Second || diff < -time.Second {
		t.Errorf("响应里的 refresh_expires_at (%s) 与库里的 (%s) 不一致", pair.RefreshExpiresAt, after)
	}
	// refresh 令牌本身不换（ADR-019 的不轮换决定没有被这批改动推翻）。
	if pair.RefreshToken != refreshTok {
		t.Error("refresh 令牌被轮换了，但本次改动只该动到期时间")
	}
}

// GREATEST 的存在理由：并发刷新里先到的那次可能算出更晚的时间戳，
// 后到的若无条件赋值就会把到期时间**往回拨**。差值只有毫秒，
// 但这类回拨正是没人能复现的「偶尔被登出」。
func TestRefreshNeverMovesExpiryBackward(t *testing.T) {
	pool := testPool(t)
	svc := &Service{repo: NewRepo(pool)}

	// 到期时间已经比 now+TTL 还远（模拟另一次刷新刚把它推得更远，或人工延长过）。
	far := time.Now().Add(refreshTokenTTL + 24*time.Hour)
	refreshTok, sessID := seedSession(t, pool, far)

	if _, err := svc.Refresh(context.Background(), refreshTok); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	after := refreshExpiryOf(t, pool, sessID)

	if after.Before(far.Add(-time.Second)) {
		t.Errorf("到期时间被往回拨了：原 %s，刷新后 %s", far, after)
	}
}

// 已过期的会话不该因为「刷新一下」就复活 —— 滑动窗口延长的是活着的会话，
// 不是给死会话一条无需重新认证的复生路径。findSession 的 expCol > now() 守着这条，
// 这个测试钉住它不被顺手改掉。
func TestRefreshRejectsAlreadyExpiredSession(t *testing.T) {
	pool := testPool(t)
	svc := &Service{repo: NewRepo(pool)}

	refreshTok, _ := seedSession(t, pool, time.Now().Add(-time.Second))

	if _, err := svc.Refresh(context.Background(), refreshTok); err == nil {
		t.Fatal("已过期的会话不应刷新成功")
	}
}
