package subscriptions

import (
	"context"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// Service 处理统一的订阅事件：验签与归一化在 handler / parser 侧完成，
// 这里只负责落库与档位切换。
//
// ADR-075 之后它**不再触碰钱包** —— 基础订阅不赠送 credits，
// credits 与存储是各自独立购买的商品。这个包因此没有任何跨 feature 依赖。
type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo}
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
