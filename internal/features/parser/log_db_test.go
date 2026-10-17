package parser

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/infra/db"
)

// 命中缓存的记录要指回写入缓存的那次调用：之前最近一次非缓存、非合并、取到了内容的记录。
// 判定落在 SQL 上，纯 Go 单测碰不到。CI 没有 Postgres，未设变量即跳过：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/parser/ -run LogListCacheSource -v
func TestLogListCacheSource(t *testing.T) {
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

	// 采样要挂在一个接口上，借迁移种下的抖音接口用
	var endpointID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM api_endpoints WHERE slug = 'tikhub_douyin_app_v3_fetch_one_video'`).
		Scan(&endpointID); err != nil {
		t.Fatalf("取种子接口失败: %v", err)
	}
	var sampleID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO api_samples (endpoint_id, kind, http_status) VALUES ($1, 'live', 200) RETURNING id`,
		endpointID).Scan(&sampleID); err != nil {
		t.Fatalf("建采样失败: %v", err)
	}

	repo := NewLogRepo(pool)
	// 随机平台名圈住本轮的记录，测试库里别的行不会混进来
	provider := "test_log_" + uuid.NewString()[:8]
	t0 := time.Now().Add(-2 * time.Hour)
	write := func(e LogEntry, at time.Duration) uuid.UUID {
		t.Helper()
		e.Provider, e.ResourceID = provider, "r1"
		if err := repo.Write(ctx, &e); err != nil {
			t.Fatalf("写记录失败: %v", err)
		}
		// created_at 由库填 now()，显式拉开间隔，不靠两次写入落在不同微秒上
		if _, err := pool.Exec(ctx, `UPDATE parse_logs SET created_at = $2 WHERE id = $1`, e.ID, t0.Add(at)); err != nil {
			t.Fatalf("改时间失败: %v", err)
		}
		return e.ID
	}
	slug := "tikhub_douyin_app_v3_fetch_one_video"
	okAttempt := func(sample *uuid.UUID) []Attempt {
		return []Attempt{{Seq: 1, Source: slug, Outcome: AttemptOK, SampleID: sample}}
	}

	// 结算失败的那次也把内容写进了缓存，照样是来源
	first := write(LogEntry{Status: LogUnpaid, Source: slug, Attempts: okAttempt(&sampleID)}, 0)
	hit1 := write(LogEntry{Status: LogOK, Cached: true}, 30*time.Second)
	write(LogEntry{Status: LogFailed, Attempts: []Attempt{{Seq: 1, Source: slug, Outcome: AttemptFailed}}}, time.Hour)
	second := write(LogEntry{Status: LogOK, Source: slug, Attempts: okAttempt(nil)}, 64*time.Minute)
	write(LogEntry{Status: LogOK, Shared: true, Source: slug}, 65*time.Minute) // 合并请求没写缓存
	hit2 := write(LogEntry{Status: LogOK, Cached: true}, 66*time.Minute)

	logs, err := repo.List(ctx, LogFilter{Provider: provider})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[uuid.UUID]LogEntry{}
	for _, l := range logs {
		byID[l.ID] = l
	}
	if len(byID) != 6 {
		t.Fatalf("应列出 6 条，得到 %d", len(byID))
	}

	if got := byID[hit1]; got.CacheSourceID == nil || *got.CacheSourceID != first ||
		got.CacheSampleID == nil || *got.CacheSampleID != sampleID || got.CacheSourceAt == nil {
		t.Errorf("第一次命中应指向 unpaid 那次及其采样: %+v", got)
	}
	// 跳过失败的那次和合并请求；来源调用没有采样（已过保留期）时只给时间
	if got := byID[hit2]; got.CacheSourceID == nil || *got.CacheSourceID != second || got.CacheSampleID != nil {
		t.Errorf("第二次命中应指向 64 分钟那次: %+v", got)
	}
	for id, l := range byID {
		if !l.Cached && l.CacheSourceID != nil {
			t.Errorf("非缓存记录不该带缓存来源: %s %+v", id, l)
		}
	}
}
