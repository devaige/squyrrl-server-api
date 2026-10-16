package parser

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/pricing"
)

// 解析记录（ADR-080）：每次 /uris/parse 一行，每次上游尝试一行。
//
// 用途只有两个 —— 核对成本收益、排查解析失败。所以**不记用户身份**：
// 谁收藏了什么与这两件事都无关。它也不是解析缓存：缓存给用户返回内容，
// 记录给管理后台对账，两者生命周期与读者都不同。

const (
	LogOK     = "ok"     // 返回了内容并收了钱
	LogFailed = "failed" // 整条链没拿到内容，不收钱
	LogUnpaid = "unpaid" // 拿到了内容（成本已发生），结算时余额已被并发请求花掉

	AttemptOK      = "ok"
	AttemptFailed  = "failed"
	AttemptSkipped = "skipped"
	AttemptPartial = "partial" // 能用但缺东西（如视频没有文件），链继续，都失败时用它兜底
)

// LogEntry 是一次解析请求的记录。
type LogEntry struct {
	ID             uuid.UUID `json:"id"`
	Provider       string    `json:"provider"`
	ResourceID     string    `json:"resource_id"`
	Status         string    `json:"status"`
	Cached         bool      `json:"cached"`
	Shared         bool      `json:"shared"` // 搭了同资源并发请求的上游调用，自身无成本
	Forced         bool      `json:"forced"`
	CreditsCharged int64     `json:"credits_charged"`
	RevenueMicros  int64     `json:"revenue_micros"` // = credits × 100，按代币面值折算
	CostMicros     int64     `json:"cost_micros"`
	Source         string    `json:"source"`
	Error          string    `json:"error"`
	LatencyMs      int       `json:"latency_ms"`
	CreatedAt      time.Time `json:"created_at"`
	Attempts       []Attempt `json:"attempts"`
}

// Attempt 是兜底链上的一次尝试（含被跳过的）。
type Attempt struct {
	Seq          int        `json:"seq"`
	Source       string     `json:"source"` // builtin:<平台> 或接口 slug
	EndpointID   *uuid.UUID `json:"endpoint_id,omitempty"`
	AccountID    *uuid.UUID `json:"account_id,omitempty"`
	AccountName  string     `json:"account_name,omitempty"`
	Outcome      string     `json:"outcome"`
	ErrorKind    string     `json:"error_kind,omitempty"`
	Error        string     `json:"error,omitempty"`
	HTTPStatus   int        `json:"http_status,omitempty"`
	LatencyMs    int        `json:"latency_ms"`
	CostMicros   int64      `json:"cost_micros"`
	BalanceAfter *int64     `json:"balance_after_micros,omitempty"`
}

type LogRepo struct {
	pool *pgxpool.Pool
}

func NewLogRepo(pool *pgxpool.Pool) *LogRepo { return &LogRepo{pool: pool} }

// Write 在一个事务里落一条记录与它的全部尝试。
func (r *LogRepo) Write(ctx context.Context, e *LogEntry) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 已提交时 Rollback 是空操作

	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO parse_logs (provider, resource_id, status, cached, shared, forced,
		                        credits_charged, cost_micros, source, error, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		e.Provider, e.ResourceID, e.Status, e.Cached, e.Shared, e.Forced,
		e.CreditsCharged, e.CostMicros, e.Source, e.Error, e.LatencyMs,
	).Scan(&id); err != nil {
		return err
	}
	if len(e.Attempts) > 0 {
		batch := &pgx.Batch{}
		for _, a := range e.Attempts {
			batch.Queue(`
				INSERT INTO parse_attempts (log_id, seq, source, endpoint_id, account_id, outcome,
				                            error_kind, error, http_status, latency_ms, cost_micros, balance_after_micros)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				id, a.Seq, a.Source, a.EndpointID, a.AccountID, a.Outcome,
				a.ErrorKind, a.Error, a.HTTPStatus, a.LatencyMs, a.CostMicros, a.BalanceAfter)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	e.ID = id
	return nil
}

// LogFilter 是后台列表的筛选条件。游标按 (created_at, id) 倒序翻页。
type LogFilter struct {
	Provider string
	Status   string
	Source   string // 产出内容的来源，或任一尝试的来源
	Before   *time.Time
	BeforeID *uuid.UUID
	Limit    int
}

// List 按时间倒序返回记录，连同各自的尝试。
func (r *LogRepo) List(ctx context.Context, f LogFilter) ([]LogEntry, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, provider, resource_id, status, cached, shared, forced,
		       credits_charged, cost_micros, source, error, latency_ms, created_at
		FROM parse_logs l
		WHERE ($1 = '' OR provider = $1)
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR source = $3 OR EXISTS (
		        SELECT 1 FROM parse_attempts a WHERE a.log_id = l.id AND a.source = $3))
		  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5::uuid))
		ORDER BY created_at DESC, id DESC
		LIMIT $6`,
		f.Provider, f.Status, f.Source, f.Before, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogEntry
	idx := map[uuid.UUID]int{}
	var ids []uuid.UUID
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.Provider, &e.ResourceID, &e.Status, &e.Cached, &e.Shared, &e.Forced,
			&e.CreditsCharged, &e.CostMicros, &e.Source, &e.Error, &e.LatencyMs, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.RevenueMicros = e.CreditsCharged * pricing.MicrosPerCredit
		e.Attempts = []Attempt{}
		idx[e.ID] = len(out)
		ids = append(ids, e.ID)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	arows, err := r.pool.Query(ctx, `
		SELECT a.log_id, a.seq, a.source, a.endpoint_id, a.account_id, COALESCE(acc.name, ''),
		       a.outcome, a.error_kind, a.error, a.http_status, a.latency_ms, a.cost_micros, a.balance_after_micros
		FROM parse_attempts a
		LEFT JOIN api_accounts acc ON acc.id = a.account_id
		WHERE a.log_id = ANY($1)
		ORDER BY a.log_id, a.seq`, ids)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var logID uuid.UUID
		var a Attempt
		if err := arows.Scan(&logID, &a.Seq, &a.Source, &a.EndpointID, &a.AccountID, &a.AccountName,
			&a.Outcome, &a.ErrorKind, &a.Error, &a.HTTPStatus, &a.LatencyMs, &a.CostMicros, &a.BalanceAfter); err != nil {
			return nil, err
		}
		i := idx[logID]
		out[i].Attempts = append(out[i].Attempts, a)
	}
	return out, arows.Err()
}

// ProviderSummary 是一个平台在统计窗口内的汇总。
type ProviderSummary struct {
	Provider      string `json:"provider"`
	Total         int64  `json:"total"`
	OK            int64  `json:"ok"`
	Failed        int64  `json:"failed"`
	Unpaid        int64  `json:"unpaid"`
	Cached        int64  `json:"cached"`
	Credits       int64  `json:"credits"`
	RevenueMicros int64  `json:"revenue_micros"`
	CostMicros    int64  `json:"cost_micros"`
}

// SourceSummary 是一个来源（内置或接口）在统计窗口内的尝试汇总。
type SourceSummary struct {
	Source      string `json:"source"`
	AccountName string `json:"account_name"`
	Attempts    int64  `json:"attempts"`
	OK          int64  `json:"ok"`
	Failed      int64  `json:"failed"`
	Skipped     int64  `json:"skipped"`
	Partial     int64  `json:"partial"`
	CostMicros  int64  `json:"cost_micros"`
}

// Summary 汇总最近 days 天：按平台看收入与成本，按来源看成功率与成本。
func (r *LogRepo) Summary(ctx context.Context, days int) ([]ProviderSummary, []SourceSummary, error) {
	since := time.Now().AddDate(0, 0, -days)

	rows, err := r.pool.Query(ctx, `
		SELECT provider, COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'ok'),
		       COUNT(*) FILTER (WHERE status = 'failed'),
		       COUNT(*) FILTER (WHERE status = 'unpaid'),
		       COUNT(*) FILTER (WHERE cached),
		       COALESCE(SUM(credits_charged), 0),
		       COALESCE(SUM(cost_micros), 0)
		FROM parse_logs
		WHERE created_at >= $1
		GROUP BY provider
		ORDER BY provider`, since)
	if err != nil {
		return nil, nil, err
	}
	providers := []ProviderSummary{}
	for rows.Next() {
		var s ProviderSummary
		if err := rows.Scan(&s.Provider, &s.Total, &s.OK, &s.Failed, &s.Unpaid, &s.Cached,
			&s.Credits, &s.CostMicros); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.RevenueMicros = s.Credits * pricing.MicrosPerCredit
		providers = append(providers, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	srows, err := r.pool.Query(ctx, `
		SELECT a.source, COALESCE(MAX(acc.name), ''), COUNT(*),
		       COUNT(*) FILTER (WHERE a.outcome = 'ok'),
		       COUNT(*) FILTER (WHERE a.outcome = 'failed'),
		       COUNT(*) FILTER (WHERE a.outcome = 'skipped'),
		       COUNT(*) FILTER (WHERE a.outcome = 'partial'),
		       COALESCE(SUM(a.cost_micros), 0)
		FROM parse_attempts a
		JOIN parse_logs l ON l.id = a.log_id
		LEFT JOIN api_accounts acc ON acc.id = a.account_id
		WHERE l.created_at >= $1
		GROUP BY a.source
		ORDER BY a.source`, since)
	if err != nil {
		return nil, nil, err
	}
	defer srows.Close()
	sources := []SourceSummary{}
	for srows.Next() {
		var s SourceSummary
		if err := srows.Scan(&s.Source, &s.AccountName, &s.Attempts, &s.OK, &s.Failed, &s.Skipped,
			&s.Partial, &s.CostMicros); err != nil {
			return nil, nil, err
		}
		sources = append(sources, s)
	}
	return providers, sources, srows.Err()
}
