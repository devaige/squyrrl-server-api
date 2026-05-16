package page

import (
	"context"

	"github.com/google/uuid"
)

// Service 现阶段是 repo 的薄包装；后续接入订阅档位限制时这里会长肉
type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Page, error) {
	return s.repo.Create(ctx, userID, in)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Page, error) {
	return s.repo.Get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Page, error) {
	return s.repo.List(ctx, userID)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Page, error) {
	return s.repo.Update(ctx, userID, id, in)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.repo.SoftDelete(ctx, userID, id)
}
