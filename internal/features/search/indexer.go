package search

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/snippet"
)

// MeiliIndexer 把 snippet.Indexer 接口落到 Meili 上。
// 上游事件触发：snippet Create/Update/Delete 后调用对应钩子；本实现保持错误自吞，
// 因为搜索索引落后于主库是可容忍的（下次 reindex 修复）。
type MeiliIndexer struct {
	m *Meili
}

func NewMeiliIndexer(m *Meili) *MeiliIndexer { return &MeiliIndexer{m: m} }

func (i *MeiliIndexer) OnUpsert(ctx context.Context, userID uuid.UUID, s *snippet.Snippet) {
	if !i.m.Enabled() || s == nil {
		return
	}
	if s.DeletedAt != nil {
		i.OnDelete(ctx, userID, s.ID)
		return
	}
	doc := Document{
		ID:        s.ID.String(),
		UserID:    userID.String(),
		Type:      s.Type,
		UpdatedAt: s.UpdatedAt,
	}
	if s.Title != nil {
		doc.Title = *s.Title
	}
	if s.Description != nil {
		doc.Description = *s.Description
	}
	if s.PageID != nil {
		doc.PageID = s.PageID.String()
	}
	if len(s.TagIDs) > 0 {
		doc.TagIDs = make([]string, 0, len(s.TagIDs))
		for _, t := range s.TagIDs {
			doc.TagIDs = append(doc.TagIDs, t.String())
		}
	}
	if err := i.m.Upsert(ctx, []Document{doc}); err != nil {
		slog.Warn("meili upsert failed", "snippet", s.ID, "err", err)
	}
}

func (i *MeiliIndexer) OnDelete(ctx context.Context, userID, id uuid.UUID) {
	if !i.m.Enabled() {
		return
	}
	if err := i.m.Delete(ctx, id); err != nil {
		slog.Warn("meili delete failed", "snippet", id, "err", err)
	}
}

func (i *MeiliIndexer) SearchIDs(ctx context.Context, userID uuid.UUID, q string, limit int) ([]uuid.UUID, error) {
	if !i.m.Enabled() || strings.TrimSpace(q) == "" {
		return nil, nil
	}
	return i.m.SearchIDs(ctx, userID, q, limit)
}
