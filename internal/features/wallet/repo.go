package wallet

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// ActivePlan 取用户当前活跃的 plan 订阅 tier；没有则视为 free
func (r *Repo) ActivePlan(ctx context.Context, userID uuid.UUID) (string, error) {
	var tier string
	err := r.pool.QueryRow(ctx, `
		SELECT tier FROM subscriptions
		WHERE user_id = $1 AND kind = 'plan' AND status = 'active'
		ORDER BY current_period_end DESC
		LIMIT 1`,
		userID).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "free", nil
	}
	return tier, err
}

// CreditsBalance 用 SUM(delta) 直接算总余额；避免维护 balance_after 的并发难度
func (r *Repo) CreditsBalance(ctx context.Context, userID uuid.UUID) (int64, error) {
	var bal int64
	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM credits_ledger WHERE user_id = $1`,
		userID,
	).Scan(&bal)
	return bal, err
}

// Grant 落一笔 credits 流水。balance_after 用最新 SUM(delta)+delta 计算；
// 并发场景下两笔可能算到同一个旧 SUM 上，导致两行 balance_after 相同——这是诊断字段，
// 真实余额永远以 CreditsBalance(SUM) 为准。
func (r *Repo) Grant(ctx context.Context, userID uuid.UUID, delta int64, reason string) (*GrantResponse, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 行级 advisory lock，按 user_id 互斥，避免同一用户的两笔并发写出对应 balance_after 倒序
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, userID.String()); err != nil {
		return nil, err
	}

	var curBal int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM credits_ledger WHERE user_id = $1`,
		userID,
	).Scan(&curBal); err != nil {
		return nil, err
	}

	newBal := curBal + delta
	var ledgerID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO credits_ledger (user_id, delta, balance_after, reason)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		userID, delta, newBal, reason,
	).Scan(&ledgerID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &GrantResponse{
		UserID:       userID,
		Delta:        delta,
		BalanceAfter: newBal,
		LedgerID:     ledgerID,
	}, nil
}

// Consume 试扣 cost（正数）；余额不足返 ErrInsufficientCredits。
// 同样用 advisory lock 串行化，避免并发扣超账。
func (r *Repo) Consume(ctx context.Context, userID uuid.UUID, cost int64, reason string) (*GrantResponse, error) {
	if cost <= 0 {
		return nil, errors.New("cost must be > 0")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, userID.String()); err != nil {
		return nil, err
	}

	var curBal int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM credits_ledger WHERE user_id = $1`,
		userID,
	).Scan(&curBal); err != nil {
		return nil, err
	}
	if curBal < cost {
		return nil, ErrInsufficientCredits
	}

	newBal := curBal - cost
	var ledgerID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO credits_ledger (user_id, delta, balance_after, reason)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		userID, -cost, newBal, reason,
	).Scan(&ledgerID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &GrantResponse{
		UserID:       userID,
		Delta:        -cost,
		BalanceAfter: newBal,
		LedgerID:     ledgerID,
	}, nil
}
