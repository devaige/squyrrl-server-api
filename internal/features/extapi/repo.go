package extapi

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/pricing"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// ListForProvider 取某 provider 下所有 enabled 的 endpoint，按 priority 升序（兜底链顺序），
// 并附带余额（SUM(delta)）。仅供 Fetch 路径使用。
func (r *Repo) ListForProvider(ctx context.Context, provider string) ([]Endpoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT e.id, e.provider, e.vendor, e.slug, e.priority, e.enabled,
		       e.unit_price_micros, e.margin_bp, e.currency, e.config,
		       COALESCE((SELECT SUM(delta) FROM api_cost_ledger l WHERE l.endpoint_id = e.id), 0)
		FROM api_endpoints e
		WHERE e.provider = $1 AND e.enabled
		ORDER BY e.priority ASC, e.created_at ASC`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var cfg []byte
		if err := rows.Scan(&e.ID, &e.Provider, &e.Vendor, &e.Slug, &e.Priority, &e.Enabled,
			&e.UnitPriceMicros, &e.MarginBP, &e.Currency, &cfg, &e.BalanceMicros); err != nil {
			return nil, err
		}
		e.Config = json.RawMessage(cfg)
		out = append(out, e)
	}
	return out, rows.Err()
}

// List 返回全部 endpoint（跨 provider），附带余额 / 累计消耗 / 调用次数，供管理后台展示。
func (r *Repo) List(ctx context.Context) ([]Endpoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT e.id, e.provider, e.vendor, e.slug, e.priority, e.enabled,
		       e.unit_price_micros, e.margin_bp, e.currency, e.config, e.created_at, e.updated_at,
		       COALESCE(SUM(l.delta), 0),
		       COALESCE(-SUM(l.delta) FILTER (WHERE l.delta < 0), 0),
		       COUNT(l.*) FILTER (WHERE l.reason = 'call')
		FROM api_endpoints e
		LEFT JOIN api_cost_ledger l ON l.endpoint_id = e.id
		GROUP BY e.id
		ORDER BY e.provider, e.priority ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var cfg []byte
		if err := rows.Scan(&e.ID, &e.Provider, &e.Vendor, &e.Slug, &e.Priority, &e.Enabled,
			&e.UnitPriceMicros, &e.MarginBP, &e.Currency, &cfg, &e.CreatedAt, &e.UpdatedAt,
			&e.BalanceMicros, &e.SpentMicros, &e.CallCount); err != nil {
			return nil, err
		}
		e.Config = json.RawMessage(cfg)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repo) Create(ctx context.Context, in CreateEndpointInput) (*Endpoint, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	priority := in.Priority
	if priority == 0 {
		priority = 100
	}
	currency := in.Currency
	if currency == "" {
		currency = "USD"
	}
	// 与 priority / currency 同处解析缺省值，而不是靠库里的 DEFAULT：
	// 落值的那一刻就写进返回给管理后台的对象，省掉一次「建完再查一遍才知道加价多少」。
	marginBP := int32(pricing.DefaultMarginBP)
	if in.MarginBP != nil {
		marginBP = *in.MarginBP
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}

	e := &Endpoint{
		Provider: in.Provider, Vendor: in.Vendor, Slug: in.Slug,
		Priority: priority, Enabled: enabled,
		UnitPriceMicros: in.UnitPriceMicros, MarginBP: marginBP,
		Currency: currency, Config: cfg,
	}
	err := r.pool.QueryRow(ctx, `
		INSERT INTO api_endpoints (provider, vendor, slug, priority, enabled, unit_price_micros, margin_bp, currency, config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at, updated_at`,
		in.Provider, in.Vendor, in.Slug, priority, enabled, in.UnitPriceMicros, marginBP, currency, []byte(cfg),
	).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Update 用 COALESCE 做部分更新：nil 参数保持原值，无需拼动态 SQL。
func (r *Repo) Update(ctx context.Context, id uuid.UUID, in UpdateEndpointInput) error {
	var cfg []byte
	if in.Config != nil {
		cfg = []byte(*in.Config)
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE api_endpoints SET
			vendor            = COALESCE($2, vendor),
			priority          = COALESCE($3, priority),
			enabled           = COALESCE($4, enabled),
			unit_price_micros = COALESCE($5, unit_price_micros),
			margin_bp         = COALESCE($6, margin_bp),
			currency          = COALESCE($7, currency),
			config            = COALESCE($8, config)
		WHERE id = $1`,
		id, in.Vendor, in.Priority, in.Enabled, in.UnitPriceMicros, in.MarginBP, in.Currency, cfg,
	)
	return err
}

func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*Endpoint, error) {
	var e Endpoint
	var cfg []byte
	err := r.pool.QueryRow(ctx, `
		SELECT id, provider, vendor, slug, priority, enabled, unit_price_micros, margin_bp, currency, config, created_at, updated_at
		FROM api_endpoints WHERE id = $1`, id).
		Scan(&e.ID, &e.Provider, &e.Vendor, &e.Slug, &e.Priority, &e.Enabled,
			&e.UnitPriceMicros, &e.MarginBP, &e.Currency, &cfg, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.Config = json.RawMessage(cfg)
	return &e, nil
}

// RecordCost 记一笔平台成本流水（追加，不改可变余额列）。
func (r *Repo) RecordCost(ctx context.Context, endpointID uuid.UUID, deltaMicros int64, reason, resourceID string) error {
	var rid *string
	if resourceID != "" {
		rid = &resourceID
	}
	_, err := r.pool.Exec(ctx,
		`INSERT INTO api_cost_ledger (endpoint_id, delta, reason, resource_id) VALUES ($1, $2, $3, $4)`,
		endpointID, deltaMicros, reason, rid)
	return err
}

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
// 核对 response_map 命中了哪些字段（见 migration 000010），那需要的是「最近几条」。
// 按天数保留在低频 endpoint 上会一条不剩，在高频 endpoint 上又会留下几十万行 ——
// 按条数两头都对，且上界与调用量完全无关。
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
