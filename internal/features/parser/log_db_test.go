package parser

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/squyrrl/api/internal/features/extapi"
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
	if err := pool.QueryRow(ctx, `INSERT INTO api_samples (endpoint_id, http_status) VALUES ($1, 200) RETURNING id`,
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

// 解析记录列表在某一步的采样和示例比出差异时带上计数，后台打开过那条采样后标为已看。
// 比对要读采样的完整响应和接口的示例，落在真库上：
//
//	SQUYRRL_TEST_DATABASE_URL='postgres://USER:PASS@HOST:5432/disposable_test_db' go test ./internal/features/parser/ -run LogsSampleDrift -v
func TestLogsSampleDrift(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	slug := "tikhub_douyin_app_v3_fetch_one_video"
	var endpointID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM api_endpoints WHERE slug = $1`, slug).Scan(&endpointID); err != nil {
		t.Fatalf("取种子接口失败: %v", err)
	}
	fixture, err := readFixture("douyin_app_v3_fetch_one_video")
	if err != nil {
		t.Fatal(err)
	}
	var samples []uuid.UUID
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_samples WHERE id = ANY($1)`, samples) })
	sample := func(status int, data string) uuid.UUID {
		t.Helper()
		resp := `{"status":` + strconv.Itoa(status) + `,"body":{"code":` + strconv.Itoa(status) + `,"data":` + data + `}}`
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO api_samples (endpoint_id, response, http_status) VALUES ($1, $2, $3) RETURNING id`,
			endpointID, resp, status).Scan(&id); err != nil {
			t.Fatalf("建采样失败: %v", err)
		}
		samples = append(samples, id)
		return id
	}
	same := sample(200, string(fixture))
	drifted := sample(200, `{"brand_new_field":1}`)
	failed := sample(400, `{"brand_new_field":1}`) // 上游失败的应答不比

	adapters := DefaultAdapters()
	ext := extapi.NewService(extapi.NewRepo(pool), extapi.Credentials{}, adapters)
	svc := NewService(Deps{Registry: NewRegistry(), Extapi: ext, Adapters: adapters, Logs: NewLogRepo(pool)})
	provider := uniqueProvider("drift")
	e := LogEntry{Provider: provider, ResourceID: "r1", Status: LogOK, Source: slug, Attempts: []Attempt{
		{Seq: 1, Source: slug, Outcome: AttemptFailed, SampleID: &failed},
		{Seq: 2, Source: slug, Outcome: AttemptFailed, SampleID: &drifted},
		{Seq: 3, Source: slug, Outcome: AttemptOK, SampleID: &same},
	}}
	if err := NewLogRepo(pool).Write(ctx, &e); err != nil {
		t.Fatalf("写记录失败: %v", err)
	}

	attempts := func() []Attempt {
		t.Helper()
		logs, err := svc.Logs(ctx, LogFilter{Provider: provider})
		if err != nil || len(logs) != 1 || len(logs[0].Attempts) != 3 {
			t.Fatalf("应列出一条带三步的记录: %v %+v", err, logs)
		}
		return logs[0].Attempts
	}
	got := attempts()
	if got[0].Drift != nil || got[2].Drift != nil {
		t.Errorf("失败应答和结构一致的采样不该带差异: %+v / %+v", got[0].Drift, got[2].Drift)
	}
	if d := got[1].Drift; d == nil || d.MissingCount == 0 || d.AddedCount != 1 || d.Viewed {
		t.Fatalf("改了结构的采样应带缺失与新增、未看: %+v", d)
	}

	if err := ext.MarkSampleViewed(ctx, drifted); err != nil {
		t.Fatalf("标记已看失败: %v", err)
	}
	if d := attempts()[1].Drift; d == nil || !d.Viewed {
		t.Errorf("看过详情后应标为已看: %+v", d)
	}
	if err := ext.MarkSampleViewed(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("不存在的采样应返回 ErrNoRows，得到 %v", err)
	}
}
