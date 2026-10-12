package users

import (
	"context"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/wallet"
)

// WalletReader 提供用户的档位与代币余额。由 wallet.Service 实现。
type WalletReader interface {
	GetWallet(ctx context.Context, userID uuid.UUID) (*wallet.Wallet, error)
}

// StorageReader 提供存储配额与占用。由 quota.Service 实现。
type StorageReader interface {
	Storage(ctx context.Context, userID uuid.UUID) (quota.StorageStatus, error)
}

// SubscriptionLister 列出仍在服务的订阅行。由 subscriptions.Service 实现。
type SubscriptionLister interface {
	Live(ctx context.Context, userID uuid.UUID) ([]subscriptions.Record, error)
}

type Service struct {
	repo    *Repo
	wallets WalletReader
	storage StorageReader
	subs    SubscriptionLister
}

func NewService(repo *Repo, wallets WalletReader, storage StorageReader, subs SubscriptionLister) *Service {
	return &Service{repo: repo, wallets: wallets, storage: storage, subs: subs}
}

// Search 查用户并逐行补上档位、代币、存储与订阅明细。
//
// 逐行调各自的读取方而不是在搜索 SQL 里一并 JOIN：档位要把 past_due 宽限期算进去，
// 那条规则只在 wallet.Repo.PlanState / quota.Service.Storage 里有，这里再写一遍
// 迟早会和用户端看到的不一致。代价是每行五次查询，由 MaxLimit 兜住上限；
// 这是后台低频接口，换得起。
func (s *Service) Search(ctx context.Context, q string, limit int) ([]Summary, error) {
	items, err := s.repo.Search(ctx, q, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	for i := range items {
		id := items[i].ID
		w, err := s.wallets.GetWallet(ctx, id)
		if err != nil {
			return nil, err
		}
		st, err := s.storage.Storage(ctx, id)
		if err != nil {
			return nil, err
		}
		subs, err := s.subs.Live(ctx, id)
		if err != nil {
			return nil, err
		}
		items[i].Plan = w.Plan
		items[i].PlanStatus = w.PlanStatus
		items[i].GraceUntil = w.GraceUntil
		items[i].CreditsBalance = w.CreditsBalance
		items[i].Storage = wallet.StorageView{QuotaBytes: st.QuotaBytes, UsedBytes: st.UsedBytes}
		items[i].Subscriptions = subs
	}
	return items, nil
}
