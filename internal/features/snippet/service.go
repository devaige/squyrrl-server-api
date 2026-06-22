package snippet

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
)

type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Snippet, error) {
	if err := s.repo.EnsurePageOwned(ctx, userID, in.PageID); err != nil {
		return nil, err
	}
	if err := s.repo.EnsureTagsOwned(ctx, userID, in.TagIDs); err != nil {
		return nil, err
	}
	if err := s.repo.EnsureFilesExist(ctx, collectFileIDs(in.Elements)); err != nil {
		return nil, err
	}
	return s.repo.Create(ctx, userID, in)
}

func collectFileIDs(els []ElementInput) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(els))
	for _, e := range els {
		ids = append(ids, e.FileID)
	}
	return ids
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Snippet, error) {
	return s.repo.Get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, in *ListInput) (*ListOutput, error) {
	items, err := s.repo.List(ctx, userID, in)
	if err != nil {
		return nil, err
	}
	out := &ListOutput{Items: items}
	if items == nil {
		out.Items = []Snippet{}
	}
	// 同步流（升序）下，把最后一项的 updated_at 作为下一次请求的 since
	if in.Since != nil && len(items) > 0 {
		last := items[len(items)-1].UpdatedAt
		out.NextSince = &last
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Snippet, error) {
	if err := s.repo.EnsurePageOwned(ctx, userID, in.PageID); err != nil {
		return nil, err
	}
	if in.TagIDs != nil {
		if err := s.repo.EnsureTagsOwned(ctx, userID, *in.TagIDs); err != nil {
			return nil, err
		}
	}
	if in.Elements != nil {
		if err := s.repo.EnsureFilesExist(ctx, collectFileIDs(*in.Elements)); err != nil {
			return nil, err
		}
	}

	res, err := s.repo.Update(ctx, userID, id, in)
	if errors.Is(err, ErrVersionConflict) {
		// ADR-002：把 loser 副本入冲突收件箱，仍然返 409 让客户端 fetch 最新版
		return nil, s.archiveConflict(ctx, userID, id, in)
	}
	return res, err
}

// archiveConflict 在版本冲突时把客户端意图写入"冲突收件箱"系统页面
// 任何归档失败都退化为普通 ErrVersionConflict（功能不被 archive 故障阻塞）
func (s *Service) archiveConflict(
	ctx context.Context,
	userID, snippetID uuid.UUID,
	in *UpdateInput,
) error {
	cur, err := s.repo.Get(ctx, userID, snippetID)
	if err != nil {
		slog.Warn("conflict 归档：无法读取 cur", "err", err)
		return ErrVersionConflict
	}
	inboxID, err := s.repo.EnsureConflictInbox(ctx, userID)
	if err != nil {
		slog.Warn("conflict 归档：找/建收件箱失败", "err", err)
		return ErrVersionConflict
	}
	loserID, err := s.repo.InsertConflictLoser(ctx, userID, inboxID, cur, in)
	if err != nil {
		slog.Warn("conflict 归档：写入失败", "err", err)
		return ErrVersionConflict
	}
	return &ConflictError{
		LoserSnippetID: loserID,
		LoserPageID:    inboxID,
		CurrentVersion: cur.Version,
	}
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID, expectedVersion int64) error {
	return s.repo.SoftDelete(ctx, userID, id, expectedVersion)
}
