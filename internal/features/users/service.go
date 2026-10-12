package users

import (
	"context"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/wallet"
)

// WalletReader 提供用户的档位与代币余额。由 wallet.Service 实现。
type WalletReader interface {
	GetWallet(ctx context.Context, userID uuid.UUID) (*wallet.Wallet, error)
}

type Service struct {
	repo    *Repo
	wallets WalletReader
}

func NewService(repo *Repo, wallets WalletReader) *Service {
	return &Service{repo: repo, wallets: wallets}
}

// Search 查用户并逐行补上钱包状态。
//
// 逐行调 GetWallet 而不是在搜索 SQL 里一并 JOIN：档位要把 past_due 宽限期算进去，
// 那条规则只在 wallet.Repo.PlanState 里有一份，这里再写一遍迟早会和用户端看到的不一致。
// 代价是每行两次查询，由 MaxLimit 兜住上限；这是后台低频接口，换得起。
func (s *Service) Search(ctx context.Context, q string, limit int) ([]Summary, error) {
	items, err := s.repo.Search(ctx, q, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	for i := range items {
		w, err := s.wallets.GetWallet(ctx, items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].Plan = w.Plan
		items[i].PlanStatus = w.PlanStatus
		items[i].GraceUntil = w.GraceUntil
		items[i].CreditsBalance = w.CreditsBalance
	}
	return items, nil
}
