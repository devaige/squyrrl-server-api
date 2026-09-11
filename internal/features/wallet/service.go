package wallet

import (
	"context"

	"github.com/google/uuid"
)

type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// ActivePlan 取用户当前生效的档位；无活跃订阅时返回 "free"。
// quota.PlanReader 的实现 —— 门槛检查每次都要用它，单独暴露避免走 GetWallet
// 白查一次余额。
func (s *Service) ActivePlan(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.repo.ActivePlan(ctx, userID)
}

func (s *Service) GetWallet(ctx context.Context, userID uuid.UUID) (*Wallet, error) {
	st, err := s.repo.PlanState(ctx, userID)
	if err != nil {
		return nil, err
	}
	bal, err := s.repo.CreditsBalance(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Wallet{
		Plan:           st.Tier,
		PlanStatus:     st.Status,
		GraceUntil:     st.GraceUntil,
		CreditsBalance: bal,
	}, nil
}

// PlanState 暴露完整的档位状态，供需要区分 active / past_due 的调用方使用。
func (s *Service) PlanState(ctx context.Context, userID uuid.UUID) (PlanState, error) {
	return s.repo.PlanState(ctx, userID)
}

// Grant 落一笔 credits 流水。idemKey 为空表示不参与幂等；
// 非空时同一个键重复调用只落一笔，后续调用返回首次结果且 Replayed=true。
func (s *Service) Grant(ctx context.Context, userID uuid.UUID, delta int64, reason, idemKey string) (*GrantResponse, error) {
	return s.repo.Grant(ctx, userID, delta, reason, idemKey)
}

// Consume 试扣 cost 个 credits；余额不足返 ErrInsufficientCredits。
func (s *Service) Consume(ctx context.Context, userID uuid.UUID, cost int64, reason string) (*GrantResponse, error) {
	return s.repo.Consume(ctx, userID, cost, reason)
}
