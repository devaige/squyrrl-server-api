package subscriptions

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/wallet"
)

// CreditGranter 落一笔 credits 流水。由 wallet.Service 实现。
type CreditGranter interface {
	Grant(ctx context.Context, userID uuid.UUID, delta int64, reason, idemKey string) (*wallet.GrantResponse, error)
}

// RestrictionReconciler 按用户当前档位重算降级后的受限标记。由 snippet.RestrictionSweeper 实现。
type RestrictionReconciler interface {
	ReconcileUser(ctx context.Context, userID uuid.UUID) error
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
	repo         *Repo
	grants       CreditGranter
	restrictions RestrictionReconciler
}

// restrictions 可以为 nil（测试）：受限标记照样由定时清扫维护，只是慢一轮。
func NewService(repo *Repo, grants CreditGranter, restrictions RestrictionReconciler) *Service {
	return &Service{repo: repo, grants: grants, restrictions: restrictions}
}

// reconcileRestrictions 档位生效后立刻重算受限标记（2026-10-20 用户决策）。
//
// 失败只记日志、不让入账失败：钱已经收了、档位已经落库，受限标记最迟在下一轮
// 定时清扫时纠正；让 webhook 因此报错只会换来渠道的重投。
func (s *Service) reconcileRestrictions(ctx context.Context, userID uuid.UUID) {
	if s.restrictions == nil {
		return
	}
	if err := s.restrictions.ReconcileUser(ctx, userID); err != nil {
		slog.Warn("档位变更后重算受限标记失败，等定时清扫", "user_id", userID, "err", err)
	}
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

	// active plan 取代之前的：同用户同时只能有一条，见 uq_subscriptions_one_active_plan。
	// 包括后台手动设置的档位 —— 用户真金白银买了一档，就该以买的为准。
	var err error
	if e.Kind == "plan" && e.Status == "active" {
		_, err = s.repo.ActivatePlan(ctx, e)
	} else {
		_, err = s.repo.Upsert(ctx, e)
	}
	if err != nil {
		return err
	}

	// 换档换了 token 的，让被换掉的那条退场。
	//
	// 放在落库**之后**：顺序反过来的话，一次落库失败会留下一个既没了
	// 旧订阅、也没有新订阅的用户 —— 而这一步只是记账，落库才是发货。
	// 与 ActivatePlan 里的退场不同，这条对 storage 同样生效，
	// 而且必须如此：plan 有唯一约束兜底，storage 的配额是 SUM，没有任何兜底。
	if e.SupersedesProviderID != "" && e.SupersedesProviderID != e.ProviderSubscriptionID {
		if err := s.repo.ExpireByProviderID(ctx, e.UserID, e.Provider, e.SupersedesProviderID); err != nil {
			return err
		}
	}
	if e.Kind == "plan" && e.Status == "active" {
		s.reconcileRestrictions(ctx, e.UserID)
	}
	return nil
}
