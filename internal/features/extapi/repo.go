package extapi

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// endpointCols 是接口连同所属账户的列，Fetch 路径与后台共用。
// 余额是账户的：同一账户下的接口共用一份钱。
const endpointCols = `
	e.id, e.account_id, e.provider, e.slug, e.priority, e.enabled,
	e.unit_price_micros, e.config, e.created_at, e.updated_at,
	a.vendor, a.slug, a.name, a.currency,
	COALESCE((SELECT SUM(delta) FROM api_cost_ledger l WHERE l.account_id = a.id), 0)`

func scanEndpoint(row pgx.Row, extra ...any) (Endpoint, error) {
	var e Endpoint
	var cfg []byte
	dest := []any{&e.ID, &e.AccountID, &e.Provider, &e.Slug, &e.Priority, &e.Enabled,
		&e.UnitPriceMicros, &cfg, &e.CreatedAt, &e.UpdatedAt,
		&e.Vendor, &e.AccountSlug, &e.AccountName, &e.Currency, &e.BalanceMicros}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return e, err
	}
	e.Config = json.RawMessage(cfg)
	return e, nil
}

func collectEndpoints(rows pgx.Rows) ([]Endpoint, error) {
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListForProvider 取某平台下所有启用的接口，按兜底链顺序：单价升序，同价看 priority。
// 免费来源排在它们之前，由 parser 负责。
func (r *Repo) ListForProvider(ctx context.Context, provider string) ([]Endpoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+endpointCols+`
		FROM api_endpoints e JOIN api_accounts a ON a.id = e.account_id
		WHERE e.provider = $1 AND e.enabled
		ORDER BY e.unit_price_micros ASC, e.priority ASC, e.created_at ASC`, provider)
	if err != nil {
		return nil, err
	}
	return collectEndpoints(rows)
}

// ListEnabled 取全部启用的接口。表很小，平台售价与解析清单都从这一份算。
func (r *Repo) ListEnabled(ctx context.Context) ([]Endpoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+endpointCols+`
		FROM api_endpoints e JOIN api_accounts a ON a.id = e.account_id
		WHERE e.enabled
		ORDER BY e.provider, e.unit_price_micros ASC, e.priority ASC, e.created_at ASC`)
	if err != nil {
		return nil, err
	}
	return collectEndpoints(rows)
}

// List 返回全部接口，附带接口自身的调用次数与累计成本，供管理后台展示。
func (r *Repo) List(ctx context.Context) ([]Endpoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+endpointCols+`,
		       COALESCE((SELECT COUNT(*) FROM api_cost_ledger l WHERE l.endpoint_id = e.id AND l.reason = 'call'), 0),
		       COALESCE((SELECT -SUM(delta) FROM api_cost_ledger l WHERE l.endpoint_id = e.id AND l.reason = 'call'), 0)
		FROM api_endpoints e JOIN api_accounts a ON a.id = e.account_id
		ORDER BY e.provider, e.unit_price_micros ASC, e.priority ASC, e.created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var calls, spent int64
		e, err := scanEndpoint(rows, &calls, &spent)
		if err != nil {
			return nil, err
		}
		e.CallCount, e.SpentMicros = calls, spent
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*Endpoint, error) {
	e, err := scanEndpoint(r.pool.QueryRow(ctx, `
		SELECT `+endpointCols+`
		FROM api_endpoints e JOIN api_accounts a ON a.id = e.account_id
		WHERE e.id = $1`, id))
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *Repo) Create(ctx context.Context, provider string, in CreateEndpointInput) (uuid.UUID, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	priority := in.Priority
	if priority == 0 {
		priority = 100
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO api_endpoints (account_id, provider, slug, priority, enabled, unit_price_micros, config)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		in.AccountID, provider, in.Slug, priority, enabled, in.UnitPriceMicros, []byte(cfg),
	).Scan(&id)
	return id, err
}

// Update 用 COALESCE 做部分更新：nil 参数保持原值，无需拼动态 SQL。
func (r *Repo) Update(ctx context.Context, id uuid.UUID, in UpdateEndpointInput) error {
	var cfg []byte
	if in.Config != nil {
		cfg = []byte(*in.Config)
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE api_endpoints SET
			priority          = COALESCE($2, priority),
			enabled           = COALESCE($3, enabled),
			unit_price_micros = COALESCE($4, unit_price_micros),
			config            = COALESCE($5, config)
		WHERE id = $1`,
		id, in.Priority, in.Enabled, in.UnitPriceMicros, cfg,
	)
	return err
}

// ---- 账户 ----

const accountCols = `
	a.id, a.vendor, a.slug, a.name, a.currency, a.note, a.created_at, a.updated_at,
	COALESCE(SUM(l.delta), 0),
	COALESCE(SUM(l.delta) FILTER (WHERE l.delta > 0), 0),
	COALESCE(-SUM(l.delta) FILTER (WHERE l.delta < 0), 0),
	COUNT(l.id) FILTER (WHERE l.reason = 'call')`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Vendor, &a.Slug, &a.Name, &a.Currency, &a.Note, &a.CreatedAt, &a.UpdatedAt,
		&a.BalanceMicros, &a.ToppedUpMicros, &a.SpentMicros, &a.CallCount)
	return a, err
}

func (r *Repo) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+accountCols+`
		FROM api_accounts a LEFT JOIN api_cost_ledger l ON l.account_id = a.id
		GROUP BY a.id
		ORDER BY a.vendor, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) GetAccount(ctx context.Context, id uuid.UUID) (*Account, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, `
		SELECT `+accountCols+`
		FROM api_accounts a LEFT JOIN api_cost_ledger l ON l.account_id = a.id
		WHERE a.id = $1
		GROUP BY a.id`, id))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *Repo) CreateAccount(ctx context.Context, in CreateAccountInput) (uuid.UUID, error) {
	currency := in.Currency
	if currency == "" {
		currency = "USD"
	}
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO api_accounts (vendor, slug, name, currency, note)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`, in.Vendor, in.Slug, in.Name, currency, in.Note).Scan(&id)
	return id, err
}

func (r *Repo) UpdateAccount(ctx context.Context, id uuid.UUID, in UpdateAccountInput) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE api_accounts SET
			name = COALESCE($2, name),
			note = COALESCE($3, note)
		WHERE id = $1`, id, in.Name, in.Note)
	return err
}

// RecordCost 记一笔账户流水（纯追加，余额 = SUM(delta)）。endpointID 为 nil 表示账户级流水（充值/调账）。
func (r *Repo) RecordCost(ctx context.Context, accountID uuid.UUID, endpointID *uuid.UUID,
	deltaMicros int64, reason, note, resourceID string) error {
	var rid *string
	if resourceID != "" {
		rid = &resourceID
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO api_cost_ledger (account_id, endpoint_id, delta, reason, note, resource_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		accountID, endpointID, deltaMicros, reason, note, rid)
	return err
}

// ListLedger 返回账户流水，默认只看充值与调账 —— 调用成本逐笔看解析记录更有用。
func (r *Repo) ListLedger(ctx context.Context, accountID uuid.UUID, includeCalls bool, limit int) ([]LedgerEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, account_id, endpoint_id, delta, reason, note, COALESCE(resource_id, ''), created_at
		FROM api_cost_ledger
		WHERE account_id = $1 AND ($2 OR reason <> 'call')
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, accountID, includeCalls, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.AccountID, &e.EndpointID, &e.Delta, &e.Reason, &e.Note,
			&e.ResourceID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- 存档 ----

func (r *Repo) InsertSample(ctx context.Context, endpointID uuid.UUID, kind, resourceID string,
	request, response []byte, httpStatus, latencyMs int, note string) error {
	var rid, nt *string
	if resourceID != "" {
		rid = &resourceID
	}
	if note != "" {
		nt = &note
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO api_samples (endpoint_id, kind, resource_id, request, response, http_status, latency_ms, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		endpointID, kind, rid, request, response, httpStatus, latencyMs, nt)
	return err
}

func (r *Repo) ListSamples(ctx context.Context, endpointID uuid.UUID) ([]Sample, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, endpoint_id, kind, COALESCE(resource_id, ''), request, response,
		       COALESCE(http_status, 0), COALESCE(latency_ms, 0), COALESCE(note, ''), created_at
		FROM api_samples WHERE endpoint_id = $1
		ORDER BY created_at DESC LIMIT 100`, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var s Sample
		var req, resp []byte
		if err := rows.Scan(&s.ID, &s.EndpointID, &s.Kind, &s.ResourceID, &req, &resp,
			&s.HTTPStatus, &s.LatencyMs, &s.Note, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Request = json.RawMessage(req)
		s.Response = json.RawMessage(resp)
		out = append(out, s)
	}
	return out, rows.Err()
}

// PruneLiveSamples 每个 endpoint 只保留最近 keep 条 kind='live' 的采样，其余删除。
//
// 按条数而不是按天数限量：live 行只在 config.archive_live=true 时写，用途是
// 核对适配器取到了哪些字段，那需要的是「最近几条」。按天数保留在低频 endpoint
// 上会一条不剩，在高频 endpoint 上又会留下几十万行 —— 按条数两头都对，且上界
// 与调用量完全无关。
//
// 排序带 id 兜底：同毫秒写入的多条若只按 created_at 排，谁进窗口是不确定的，
// 于是两轮清理之间会反复删掉又留下不同的行（与 snippet 限制扫描同一个教训）。
//
// kind='sample' 不在范围内 —— 那是管理员手工录入的契约示例，是业务数据。
func (r *Repo) PruneLiveSamples(ctx context.Context, keep int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM api_samples s
		USING (
			SELECT id, row_number() OVER (
				PARTITION BY endpoint_id ORDER BY created_at DESC, id DESC
			) AS rn
			FROM api_samples
			WHERE kind = 'live'
		) ranked
		WHERE s.id = ranked.id AND ranked.rn > $1`, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
