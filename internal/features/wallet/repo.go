package wallet

import (
	"context"
	"errors"
	"fmt"

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

// applyDelta 算出落账后的余额，并挡住两类会写坏账本的输入。
//
// 抽成纯函数是为了可测：Grant 的其余部分都在事务与 advisory lock 里，
// 只有这段判断是纯算术，而它恰恰是唯一有边界条件的地方。
//
// 溢出必须在负余额之前判。int64 回绕会把一个巨大的正 delta 变成负数、
// 把一个巨大的负 delta 变成正数——后者正好绕过 `newBal < 0`，
// 于是「防止余额变负」的检查本身被一个更极端的输入穿过去。
func applyDelta(curBal, delta int64) (int64, error) {
	newBal := curBal + delta
	if (delta > 0 && newBal < curBal) || (delta < 0 && newBal > curBal) {
		return 0, fmt.Errorf("%w: balance=%d delta=%d", ErrGrantOverflow, curBal, delta)
	}
	// 余额不设下限时，后台手滑填一个大负数就能把账户打成负余额，
	// 此后 Consume 的 `curBal < cost` 恒成立，用户被 402 永久锁死。
	// 拒绝而不是夹到 0：夹会让流水里的 delta 与请求不一致，
	// 事后对账最难查的就是这种「看起来成功了但数字对不上」。
	if newBal < 0 {
		return 0, fmt.Errorf("%w: balance=%d delta=%d", ErrNegativeBalance, curBal, delta)
	}
	return newBal, nil
}

// Grant 落一笔 credits 流水。balance_after 用最新 SUM(delta)+delta 计算；
// 并发场景下两笔可能算到同一个旧 SUM 上，导致两行 balance_after 相同——这是诊断字段，
// 真实余额永远以 CreditsBalance(SUM) 为准。
//
// 扣减（delta 为负）不得使余额变负，见 applyDelta。该判断在 advisory lock 之内，
// 因此并发的两笔扣减不会各自读到足够余额而合起来穿透。
func (r *Repo) Grant(ctx context.Context, userID uuid.UUID, delta int64, reason, idemKey string) (*GrantResponse, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 行级 advisory lock，按 user_id 互斥，避免同一用户的两笔并发写出对应 balance_after 倒序
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, userID.String()); err != nil {
		return nil, err
	}

	// 幂等查询放在 advisory lock 之内：锁外查会让两个并发的同键请求都查空、
	// 都去落账，然后其中一个撞上唯一索引报错 —— 那是「重试就好」的错误，
	// 却会被调用方当成失败。锁内查则第二个请求直接读到第一个刚提交的结果。
	if idemKey != "" {
		if prev, err := scanExisting(ctx, tx, idemKey); err != nil {
			return nil, err
		} else if prev != nil {
			return prev, nil
		}
	}

	var curBal int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM credits_ledger WHERE user_id = $1`,
		userID,
	).Scan(&curBal); err != nil {
		return nil, err
	}

	newBal, err := applyDelta(curBal, delta)
	if err != nil {
		return nil, err
	}

	// 空字符串要落成 NULL 而不是 ''：唯一索引对 NULL 不设约束，对 '' 则视为普通值，
	// 于是所有「不需要幂等」的流水会在第二笔起互相冲突。
	var keyArg any
	if idemKey != "" {
		keyArg = idemKey
	}

	var ledgerID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO credits_ledger (user_id, delta, balance_after, reason, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, delta, newBal, reason, keyArg,
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

// scanExisting 查这个幂等键是否已经落过账。命中返回首次的结果，未命中返回 (nil, nil)。
func scanExisting(ctx context.Context, tx pgx.Tx, idemKey string) (*GrantResponse, error) {
	var (
		res GrantResponse
		bal int64
	)
	err := tx.QueryRow(ctx, `
		SELECT id, user_id, delta, balance_after
		FROM credits_ledger
		WHERE idempotency_key = $1`, idemKey,
	).Scan(&res.LedgerID, &res.UserID, &res.Delta, &bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// balance_after 是诊断列，重放时把它原样回给调用方即可 —— 真实余额永远以 SUM 为准，
	// 而重放本就不该改变任何余额。
	res.BalanceAfter = bal
	res.Replayed = true
	return &res, nil
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
		return nil, &InsufficientCreditsError{Balance: curBal, Required: cost}
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
