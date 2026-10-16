package parser

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/pricing"
)

// 解析售价按平台固定（ADR-080）。
//
// 售价要向用户公示：同一平台不管由哪个来源产出、是否命中缓存，都收同一个价。
// 让价格随实际命中的上游浮动，既解释不清，也等于把「接了哪些上游、谁先谁后」
// 写在价格上。成本差异由后台在接入上游时把关 —— 同一平台的接口单价不会差出数量级。
//
// 定价来源按优先级：
//  1. parse_prices 里的覆盖值（后台手动设定）；
//  2. 自动价：该平台已启用、有适配器的接口里**最高**的单价 × 2（pricing.CreditCost）；
//     取最高而不是最便宜的，是因为兜底链落到哪一个由运行时失败决定，售价得盖得住最坏的那次；
//  3. 只有免费来源的平台：SQUYRRL_PARSE_COST。

// PriceRepo 存取各平台的售价覆盖值。
type PriceRepo struct {
	pool *pgxpool.Pool
}

func NewPriceRepo(pool *pgxpool.Pool) *PriceRepo { return &PriceRepo{pool: pool} }

func (r *PriceRepo) Get(ctx context.Context, provider string) (int64, bool, error) {
	var credits int64
	err := r.pool.QueryRow(ctx, `SELECT credits FROM parse_prices WHERE provider = $1`, provider).Scan(&credits)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return credits, true, nil
}

func (r *PriceRepo) All(ctx context.Context) (map[string]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT provider, credits FROM parse_prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var c int64
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out[p] = c
	}
	return out, rows.Err()
}

func (r *PriceRepo) Set(ctx context.Context, provider string, credits int64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO parse_prices (provider, credits) VALUES ($1, $2)
		ON CONFLICT (provider) DO UPDATE SET credits = EXCLUDED.credits`, provider, credits)
	return err
}

func (r *PriceRepo) Delete(ctx context.Context, provider string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM parse_prices WHERE provider = $1`, provider)
	return err
}

// maxUnitPrice 取一组接口里可调用者的最高单价。
func (s AdapterSet) maxUnitPrice(eps []extapi.Endpoint) int64 {
	var top int64
	for _, ep := range eps {
		if _, ok := s.callable(ep); ok && ep.UnitPriceMicros > top {
			top = ep.UnitPriceMicros
		}
	}
	return top
}

// autoPrice 是没有覆盖值时的默认售价。
func autoPrice(maxUnitMicros, fallback int64) (int64, error) {
	if maxUnitMicros <= 0 {
		return fallback, nil
	}
	return pricing.CreditCost(maxUnitMicros, pricing.DefaultMarginBP)
}

// PriceInfo 是后台定价页的一行。
type PriceInfo struct {
	Provider           string `json:"provider"`
	HasBuiltin         bool   `json:"has_builtin"`           // 有内置免费来源
	Endpoints          int    `json:"endpoints"`             // 启用且可调用的付费接口数
	MaxUnitPriceMicros int64  `json:"max_unit_price_micros"` // 其中最高的单价
	AutoCredits        int64  `json:"auto_credits"`
	OverrideCredits    *int64 `json:"override_credits"` // nil = 未覆盖，用自动价
	EffectiveCredits   int64  `json:"effective_credits"`
	// Loss 为 true 表示售价低于最贵接口的单价：兜底落到它时每次都亏。
	Loss bool `json:"loss"`
	// Note 是平台按链接调整价格的说明（如小红书网页端链接 2 倍），没有为空。
	Note string `json:"note,omitempty"`
}
