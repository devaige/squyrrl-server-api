package snippet

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
)

// Indexer 把单条 snippet 推到外部搜索引擎（Meilisearch）。
// 故意做成接口而非直接依赖 search 包：让 snippet 层与具体引擎解耦，
// 也方便测试场景注入空实现。所有方法都是 best-effort，错误由实现内部 swallow。
type Indexer interface {
	OnUpsert(ctx context.Context, userID uuid.UUID, snip *Snippet)
	OnDelete(ctx context.Context, userID, id uuid.UUID)
}

type noopIndexer struct{}

func (noopIndexer) OnUpsert(context.Context, uuid.UUID, *Snippet) {}
func (noopIndexer) OnDelete(context.Context, uuid.UUID, uuid.UUID) {}

type Service struct {
	repo    *Repo
	indexer Indexer
}

func NewService(repo *Repo) *Service { return &Service{repo: repo, indexer: noopIndexer{}} }

// SetIndexer 由 server.go 在装配阶段注入。允许 nil（退化为 noop）
func (s *Service) SetIndexer(i Indexer) {
	if i == nil {
		s.indexer = noopIndexer{}
		return
	}
	s.indexer = i
}

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
	res, err := s.repo.Create(ctx, userID, in)
	if err == nil && res != nil {
		s.indexer.OnUpsert(ctx, userID, res)
	}
	return res, err
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
	if err == nil && res != nil {
		s.indexer.OnUpsert(ctx, userID, res)
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
	if err := s.repo.SoftDelete(ctx, userID, id, expectedVersion); err != nil {
		return err
	}
	s.indexer.OnDelete(ctx, userID, id)
	return nil
}

// SearchIDs 由 search engine 提供的命中列表过滤；返回有序 id 切片。
// 当 indexer 不是 SearchableIndexer 时返回 (nil, false)，caller 走 ILIKE fallback。
func (s *Service) SearchIDs(ctx context.Context, userID uuid.UUID, q string, limit int) ([]uuid.UUID, bool, error) {
	if si, ok := s.indexer.(SearchableIndexer); ok {
		ids, err := si.SearchIDs(ctx, userID, q, limit)
		return ids, true, err
	}
	return nil, false, nil
}

// ListByIDs 返回指定 id 列表对应的 snippet，保持 ids 给定顺序（用于 Meili 搜索相关性排序）。
func (s *Service) ListByIDs(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) ([]Snippet, error) {
	if len(ids) == 0 {
		return []Snippet{}, nil
	}
	all, err := s.repo.ListByIDs(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	// 重排序按 ids 顺序
	indexByID := make(map[uuid.UUID]int, len(ids))
	for i, id := range ids {
		indexByID[id] = i
	}
	out := make([]Snippet, len(all))
	copy(out, all)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if indexByID[out[i].ID] > indexByID[out[j].ID] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// SearchableIndexer 是可选接口：实现了它的 Indexer 才支持远端搜索查询
type SearchableIndexer interface {
	SearchIDs(ctx context.Context, userID uuid.UUID, q string, limit int) ([]uuid.UUID, error)
}
