package parser

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/infra/db"
)

// 版本化缓存的全部行为都在 SQL 里：取最新用的是 ORDER BY + LIMIT，降级护栏是
// ON CONFLICT 里的一个 CASE，版本裁剪是个窗口函数。这些在纯 Go 单测里一行都碰不到，
// 而它们错掉的表现全都是「静默返回了不对的那一行」——不报错、不崩溃，只是从此
// 永远给用户看旧内容，或者反过来把一条好缓存换成降级返回的坏缓存。所以必须打真库。
//
// 与 auth/refresh_slide_test.go 同一套约定：未设变量即跳过，不让 CI 因为缺一个
// 它从来没有的依赖而变红。本地跑：
//
//	docker compose -f docker-compose.dev.yml up -d postgres
//	SQUYRRL_TEST_DATABASE_URL='postgres://squyrrl:squyrrl_dev@localhost:55432/squyrrl?sslmode=disable' go test ./internal/features/parser/ -run Cache -v
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

// newTestCache 给出一个干净命名空间下的缓存。provider 用测试专属前缀，
// 清理只删自己写的行，避免与库里其他数据互相影响。
func newTestCache(t *testing.T, pool *pgxpool.Pool, provider string) *Cache {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM parse_cache WHERE provider = $1`, provider); err != nil {
			t.Logf("清理测试缓存失败: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return NewCache(pool, time.Hour)
}

func snip(title string) *ParsedSnippet {
	s := &ParsedSnippet{Type: "special", Subtype: "test", Payload: json.RawMessage(`{}`)}
	if title != "" {
		s.Title = &title
	}
	return s
}

// 同一资源的多个版本共存，读取取最新写入的那一版。
//
// 这是整套改动的主断言：改版前 (provider, resource_id) 是唯一键，第二个版本会直接
// 覆盖第一个；现在它们各占一行，而 Get 必须稳定地挑出最后写进来的那个。
func TestCacheKeepsVersionsAndReadsLatest(t *testing.T) {
	pool := testPool(t)
	const provider = "test_versioned"
	c := newTestCache(t, pool, provider)
	ctx := context.Background()

	for _, v := range []struct{ version, title string }{
		{"e1", "第一版"},
		{"e2", "第二版"},
		{"e3", "第三版"},
	} {
		if err := c.Put(ctx, provider, "chat:1", v.version, snip(v.title)); err != nil {
			t.Fatalf("写入 %s 失败: %v", v.version, err)
		}
		// created_at 是排序依据，同一事务内多次写入可能落在同一时间戳上；
		// 真实场景里两次解析之间必然有网络往返，这里用 sleep 还原那个间隔。
		time.Sleep(2 * time.Millisecond)
	}

	got, version, hit, err := c.Get(ctx, provider, "chat:1")
	if err != nil || !hit {
		t.Fatalf("读取失败: hit=%v err=%v", hit, err)
	}
	if version != "e3" || got.Title == nil || *got.Title != "第三版" {
		t.Fatalf("取到的不是最新版：version=%q title=%v", version, got.Title)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM parse_cache WHERE provider=$1 AND provider_resource_id='chat:1'`,
		provider).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("三个版本应各占一行，实际 %d 行", n)
	}
}

// 空 version 保持改版前的语义：每个资源恒一行，重复写入是覆盖而非追加。
// 内置的 YouTube/Gist/Reddit/GenericOG 全部落在这一档，它们不该因为这次改动而变样。
func TestCacheEmptyVersionStaysSingleRow(t *testing.T) {
	pool := testPool(t)
	const provider = "test_unversioned"
	c := newTestCache(t, pool, provider)
	ctx := context.Background()

	for _, title := range []string{"旧", "新"} {
		if err := c.Put(ctx, provider, "res:1", "", snip(title)); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM parse_cache WHERE provider=$1`, provider).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("无版本信息时应恒一行，实际 %d 行", n)
	}

	got, _, hit, err := c.Get(ctx, provider, "res:1")
	if err != nil || !hit {
		t.Fatalf("读取失败: hit=%v err=%v", hit, err)
	}
	if got.Title == nil || *got.Title != "新" {
		t.Fatalf("应取到后写入的那一份，实际 %v", got.Title)
	}
}

// 降级护栏：同一 version 下，没有 title 的新结果不得覆盖有 title 的旧结果。
//
// 同一 version 意味着上游内容逐字未变，两次解析本应一致；出现差异只可能是这次
// 上游降级了。而这张表跨用户共享，一次降级返回会让**所有**后来者拿到坏数据。
func TestCachePutRejectsDegradedPayload(t *testing.T) {
	pool := testPool(t)
	const provider = "test_degrade"
	c := newTestCache(t, pool, provider)
	ctx := context.Background()

	if err := c.Put(ctx, provider, "res:1", "v1", snip("完整标题")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 上游降级：同一版本，但标题丢了
	if err := c.Put(ctx, provider, "res:1", "v1", snip("")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, _, hit, err := c.Get(ctx, provider, "res:1")
	if err != nil || !hit {
		t.Fatalf("读取失败: hit=%v err=%v", hit, err)
	}
	if got.Title == nil || *got.Title != "完整标题" {
		t.Fatalf("降级结果不应覆盖已有的完整结果，实际 %v", got.Title)
	}

	// 反向：有 title 的新结果应当正常覆盖
	if err := c.Put(ctx, provider, "res:1", "v1", snip("更新后的标题")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, _, _, err = c.Get(ctx, provider, "res:1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == nil || *got.Title != "更新后的标题" {
		t.Fatalf("非降级结果应当覆盖，实际 %v", got.Title)
	}
}

// 版本裁剪只动超出上限的那部分，且保留的是最近抓取的那几版。
func TestCachePruneVersionsKeepsNewest(t *testing.T) {
	pool := testPool(t)
	const provider = "test_prune"
	c := newTestCache(t, pool, provider)
	ctx := context.Background()

	// 直接写库而不走 Put：要控制 created_at 才能构造出确定的新旧次序。
	for i := 1; i <= 8; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO parse_cache (provider, provider_resource_id, version, payload, created_at)
			VALUES ($1, 'res:1', $2, $3, now() - ($4 || ' minutes')::interval)`,
			provider, "v"+itoa(i), []byte(`{"title":"t`+itoa(i)+`"}`), itoa(i)); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	n, err := c.PruneVersions(ctx, 3, 1000)
	if err != nil {
		t.Fatalf("裁剪失败: %v", err)
	}
	if n != 5 {
		t.Fatalf("8 版保留 3 版应删 5 行，实际删了 %d 行", n)
	}

	rows, err := pool.Query(ctx,
		`SELECT version FROM parse_cache WHERE provider=$1 ORDER BY created_at DESC`, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, v)
	}
	// created_at 越小越新（now() - i 分钟），所以留下的应是 v1/v2/v3
	want := []string{"v1", "v2", "v3"}
	if len(kept) != len(want) {
		t.Fatalf("应保留 %v，实际 %v", want, kept)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("应保留 %v，实际 %v", want, kept)
		}
	}
}

// 过期行读不到，但它仍然占着行 —— 清理交给 Sweep，Get 只负责不返回它。
func TestCacheGetSkipsExpired(t *testing.T) {
	pool := testPool(t)
	const provider = "test_expired"
	c := newTestCache(t, pool, provider)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO parse_cache (provider, provider_resource_id, version, payload, expires_at)
		VALUES ($1, 'res:1', 'v1', '{"title":"过期"}', now() - INTERVAL '1 hour')`,
		provider); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	if _, _, hit, err := c.Get(ctx, provider, "res:1"); err != nil || hit {
		t.Fatalf("过期行不应被读到: hit=%v err=%v", hit, err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
