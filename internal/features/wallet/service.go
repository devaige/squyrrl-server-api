package wallet

import (
	"context"

	"github.com/google/uuid"
)

type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) GetWallet(ctx context.Context, userID uuid.UUID) (*Wallet, error) {
	plan, err := s.repo.ActivePlan(ctx, userID)
	if err != nil {
		return nil, err
	}
	bal, err := s.repo.CreditsBalance(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Wallet{Plan: plan, CreditsBalance: bal}, nil
}

func (s *Service) Grant(ctx context.Context, userID uuid.UUID, delta int64, reason string) (*GrantResponse, error) {
	return s.repo.Grant(ctx, userID, delta, reason)
}

// Consume 试扣 cost 个 credits；余额不足返 ErrInsufficientCredits。
func (s *Service) Consume(ctx context.Context, userID uuid.UUID, cost int64, reason string) (*GrantResponse, error) {
	return s.repo.Consume(ctx, userID, cost, reason)
}
