package subscriptions

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// querier 是 *pgxpool.Pool 与 pgx.Tx 的公共子集：同一段 SQL 既能单独跑，也能拼进事务。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Upsert 根据 (provider, provider_subscription_id) 唯一性插入或更新订阅行。
// 因 schema 上没建该唯一索引，这里手动 SELECT + UPDATE / INSERT。
//
// 不处理 plan 的唯一约束：要成为 active plan 的事件走 ActivatePlan。
func (r *Repo) Upsert(ctx context.Context, e *SubscriptionEvent) (uuid.UUID, error) {
	return upsert(ctx, r.pool, e)
}

// ActivatePlan 让 e 成为该用户唯一的 active plan：先让其它 active plan 退场，再落 e。
//
// 顺序不能反过来。uq_subscriptions_one_active_plan 只允许一条 active plan，
// 先 INSERT 的话，只要新旧两条的渠道 ID 不同（Play 换档换了 purchaseToken、
// 用户换了渠道重新买）就当场撞约束，webhook 永远失败。
// 两步放在同一事务里：退场成功而落库失败时，用户不会落到「一条 plan 都没有」。
func (r *Repo) ActivatePlan(ctx context.Context, e *SubscriptionEvent) (uuid.UUID, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	// 排除的是**同一条渠道订阅**而不是某个行 id：此刻新行可能还不存在。
	// IS DISTINCT FROM 而不是 <>：admin 行的渠道 ID 是 NULL，<> 对 NULL 不成立，
	// 那些行会被漏掉，随后的 INSERT 照样撞约束。
	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'expired', updated_at = now()
		WHERE user_id = $1
		  AND kind = 'plan'
		  AND status = 'active'
		  AND (payment_provider <> $2
		       OR payment_provider_subscription_id IS DISTINCT FROM $3)`,
		e.UserID, string(e.Provider), e.ProviderSubscriptionID); err != nil {
		return uuid.Nil, err
	}

	id, err := upsert(ctx, tx, e)
	if err != nil {
		return uuid.Nil, err
	}
	return id, tx.Commit(ctx)
}

func upsert(ctx context.Context, q querier, e *SubscriptionEvent) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM subscriptions
		WHERE payment_provider = $1 AND payment_provider_subscription_id = $2
		LIMIT 1`,
		string(e.Provider), e.ProviderSubscriptionID,
	).Scan(&id)

	if err == nil {
		// update
		_, err = q.Exec(ctx, `
			UPDATE subscriptions SET
				kind = $1,
				tier = $2,
				billing_period = $3,
				status = $4,
				current_period_start = $5,
				current_period_end = $6,
				canceled_at = $7,
				bonus_storage_gb = $8,
				updated_at = now()
			WHERE id = $9`,
			e.Kind, e.Tier, e.BillingPeriod, e.Status,
			e.PeriodStart, e.PeriodEnd, e.CanceledAt, e.BonusStorageGB, id)
		return id, err
	}

	// insert
	err = q.QueryRow(ctx, `
		INSERT INTO subscriptions (
			user_id, kind, tier, billing_period, status,
			current_period_start, current_period_end,
			payment_provider, payment_provider_subscription_id,
			canceled_at, bonus_storage_gb)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id`,
		e.UserID, e.Kind, e.Tier, e.BillingPeriod, e.Status,
		e.PeriodStart, e.PeriodEnd,
		string(e.Provider), e.ProviderSubscriptionID,
		e.CanceledAt, e.BonusStorageGB,
	).Scan(&id)
	return id, err
}

// ExpireByProviderID 把渠道侧的某一条订阅标为 expired。
//
// 供 Play 的 `linkedPurchaseToken` 用：换档会换一个新 purchaseToken，而被换掉的
// 那条**不会另外收到过期通知**。不标它的后果不是少给而是多给 —— 存储配额是
// 所有 active 行的 SUM，旧行留在那里等于用户付一份钱拿两份空间，
// 且不会自愈（那个 SUM 只看 status，不看 current_period_end）。
//
// 限定同一 provider 与同一 user：purchaseToken 只在 Google 的命名空间里唯一，
// 而「取代」永远发生在同一个账号内部。跨出这两条的匹配一律是错的。
func (r *Repo) ExpireByProviderID(ctx context.Context, userID uuid.UUID, provider Provider, providerID string) error {
	if providerID == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'expired', updated_at = now()
		WHERE user_id = $1
		  AND payment_provider = $2
		  AND payment_provider_subscription_id = $3
		  AND status <> 'expired'`,
		userID, string(provider), providerID)
	return err
}
