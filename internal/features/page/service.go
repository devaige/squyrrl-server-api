package page

import (
	"context"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/quota"
)

type Service struct {
	repo  *Repo
	quota *quota.Service
}

func NewService(repo *Repo, q *quota.Service) *Service {
	return &Service{repo: repo, quota: q}
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Page, error) {
	if err := s.quota.CheckPageCreate(ctx, userID); err != nil {
		return nil, err
	}
	// 隐藏页面是档位能力（standard 起），与页面总数是两条独立门槛：
	// 额度没满但档位不够时，要告诉用户「升级才能隐藏」而不是「页面太多」。
	if in.IsHidden {
		if err := s.quota.CheckHiddenPage(ctx, userID); err != nil {
			return nil, err
		}
	}
	return s.repo.Create(ctx, userID, in)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Page, error) {
	return s.repo.Get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Page, error) {
	return s.repo.List(ctx, userID)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Page, error) {
	// 把已有页面改成隐藏，与新建一个隐藏页面是同一件事，门槛必须一致 ——
	// 只在 Create 上设卡的话，「先建普通页、再 PATCH 成隐藏」就是一条完整的绕过路径。
	// 反向（取消隐藏）不设限：降级用户应当能把自己的隐藏页改回可见。
	if in.IsHidden != nil && *in.IsHidden {
		if err := s.quota.CheckHiddenPage(ctx, userID); err != nil {
			return nil, err
		}
	}
	return s.repo.Update(ctx, userID, id, in)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.repo.SoftDelete(ctx, userID, id)
}
