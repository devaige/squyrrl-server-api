package subscriptions

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Upsert 根据 (provider, provider_subscription_id) 唯一性插入或更新订阅行。
// 因 schema 上没建该唯一索引，这里手动 SELECT + UPDATE / INSERT。
//
// 同一用户的 active plan 受 uq_subscriptions_one_active_plan 部分唯一约束保护，
// 所以「升级」场景需要先把旧 plan status 改成 canceled / expired 再 INSERT 新 plan。
// 这部分由 service 层负责协调，repo 只做无脑落库。
func (r *Repo) Upsert(ctx context.Context, e *SubscriptionEvent) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM subscriptions
		WHERE payment_provider = $1 AND payment_provider_subscription_id = $2
		LIMIT 1`,
		string(e.Provider), e.ProviderSubscriptionID,
	).Scan(&id)

	if err == nil {
		// update
		_, err = r.pool.Exec(ctx, `
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
	err = r.pool.QueryRow(ctx, `
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

// SupersedeActivePlan 把同用户下其它 active plan 标为 expired，给新 plan 让路。
// 在跨等级升降级场景下避免触发 uq_subscriptions_one_active_plan 唯一冲突。
func (r *Repo) SupersedeActivePlan(ctx context.Context, userID uuid.UUID, keepID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status = 'expired', updated_at = now()
		WHERE user_id = $1
		  AND kind = 'plan'
		  AND status = 'active'
		  AND id <> $2`,
		userID, keepID)
	return err
}
