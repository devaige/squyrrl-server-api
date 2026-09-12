package subscriptions

import (
	"context"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/wallet"
)

// CreditGranter 落一笔 credits 流水。由 wallet.Service 实现。
type CreditGranter interface {
	Grant(ctx context.Context, userID uuid.UUID, delta int64, reason, idemKey string) (*wallet.GrantResponse, error)
}

// Service 处理统一的订阅事件：验签与归一化在 handler / parser 侧完成，
// 这里只负责落库与档位切换。
//
// ADR-075 拆商品时这个包与钱包**完全解耦**过一阵：基础订阅不再赠送 credits。
// 现在钱包又回来了，但原因完全不同 —— 代币本身成了一件可购买的商品，
// 它的支付回调恰好走同一个 webhook 入口。区别要记牢：
// 那时是「订阅事件顺带发币」（而 customer.subscription.updated 在换卡、
// 改 metadata 时同样触发，于是每次都重发一整月额度）；
// 现在是「一次真实结账发一次币」，且带幂等键。
type Service struct {
	repo   *Repo
	grants CreditGranter
}

func NewService(repo *Repo, grants CreditGranter) *Service {
	return &Service{repo: repo, grants: grants}
}

// ApplyPurchase 把一次性支付落成 credits 流水。
//
// 幂等键带 provider 前缀，见 OneTimePurchase.IdempotencyKey —— 支付网关重投
// 是常态，同一笔支付到达两次必须只发一次币。
func (s *Service) ApplyPurchase(ctx context.Context, p *OneTimePurchase) error {
	if s.grants == nil {
		return ErrUnknownEvent
	}
	_, err := s.grants.Grant(ctx, p.UserID, p.Credits,
		"purchase:"+string(p.Provider), p.IdempotencyKey())
	return err
}

func (s *Service) Apply(ctx context.Context, e *SubscriptionEvent) error {
	// tier 来自三家渠道的商品 ID / metadata，属于外部输入，必须校验。
	// 校验只针对 active 的 plan：过期或取消事件即便带着已下架的 tier 也应当照常落库，
	// 否则一个下线的商品会让它的退订事件永远失败、订阅卡在 active。
	if e.Kind == "plan" && e.Status == "active" && !entitlement.Known(e.Tier) {
		return ErrUnknownTier
	}

	id, err := s.repo.Upsert(ctx, e)
	if err != nil {
		return err
	}

	// active plan 取代之前的：同用户同时只能有一条，见 uq_subscriptions_one_active_plan
	if e.Kind == "plan" && e.Status == "active" {
		if err := s.repo.SupersedeActivePlan(ctx, e.UserID, id); err != nil {
			return err
		}
	}
	return nil
}
