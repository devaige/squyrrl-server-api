package subscriptions

import (
	"context"
	"log/slog"

	"github.com/squyrrl/api/internal/features/wallet"
)

// Service 处理统一的订阅事件，落库 + 触发 credits 发放。
// 输入是已经验签并归一化好的 SubscriptionEvent，输出是落库后的 subscription id。
type Service struct {
	repo      *Repo
	walletSvc *wallet.Service
}

func NewService(repo *Repo, walletSvc *wallet.Service) *Service {
	return &Service{repo: repo, walletSvc: walletSvc}
}

func (s *Service) Apply(ctx context.Context, e *SubscriptionEvent) error {
	if e.Kind == "plan" {
		if _, ok := CreditsByTier[e.Tier]; !ok && e.Status == "active" {
			return ErrUnknownTier
		}
	}

	id, err := s.repo.Upsert(ctx, e)
	if err != nil {
		return err
	}

	// active plan：替换之前的，并按 tier 入账 credits
	if e.Kind == "plan" && e.Status == "active" {
		if err := s.repo.SupersedeActivePlan(ctx, e.UserID, id); err != nil {
			return err
		}
		credits := CreditsByTier[e.Tier]
		if credits > 0 {
			reason := "subscription_" + string(e.Provider) + "_" + e.Tier
			if _, err := s.walletSvc.Grant(ctx, e.UserID, credits, reason); err != nil {
				// credits 落账失败不阻断订阅本身（已落库）；记日志由上层处理
				slog.Warn("subscription credits grant failed", "user", e.UserID, "err", err)
			}
		}
	}
	return nil
}
