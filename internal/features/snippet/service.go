package snippet

import (
	"context"
	"errors"
	"log/slog"

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

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Snippet, error) {
	// 门槛检查放在所有权校验之前：配额满了就没必要再查页面/标签/文件归属，
	// 而且「配额已满」比「页面不属于你」是更根本的拒绝理由，先报它更不容易误导。
	if err := s.quota.CheckSnippetCreate(ctx, userID); err != nil {
		return nil, err
	}
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
	res, err := s.repo.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	// 冻结期只挡详情，不挡列表 —— 列表里仍然列出这些条目（用户决策）。
	// 宽限期照常可读：那一段的限制只是「不可修改」。
	if st := res.Restriction(); st == RestrictionFrozen {
		plan, _ := s.quota.PlanOf(ctx, userID)
		return nil, restrictedError(
			"该碎片已冻结：恢复订阅后可继续访问，冻结期结束将永久删除",
			plan, *res.RestrictedAt, st)
	}
	return res, nil
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
	// 受限碎片一律不可修改，宽限期与冻结期都拒。
	//
	// 判断放在最前面而不是交给 repo.Update 的 WHERE：那样会退化成
	// 「找不到行」→ 404 或版本冲突，而用户真正需要知道的是「这条超出了你的方案」。
	// 一个能操作却给出错误原因的界面，比一个假装东西不存在的界面好得多。
	cur, err := s.repo.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if st := cur.Restriction(); st != RestrictionNone {
		plan, _ := s.quota.PlanOf(ctx, userID)
		return nil, restrictedError(
			"这条碎片超出当前方案，暂时不可修改；删除仍然可用",
			plan, *cur.RestrictedAt, st)
	}

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
