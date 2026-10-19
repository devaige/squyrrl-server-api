// Package replica 是原生客户端的增量同步（ADR-084）：设备本机存一份完整数据，
// 后台用 /sync/pull 拉取别处的改动、用 /sync/push 推送本机的改动。
//
// 拉取的游标是服务端给每个账号分配的递增号 sync_seq，从不使用墙上时间 ——
// 客户端时钟可以是错的。推送按实体逐条处理，各自一个事务、各自一个结果，
// 一条失败不影响同一批里的其它条。具体的写入规则在 snippet / page / tag 各包里。
//
// 网页端、浏览器扩展没有持久的本机存储，仍走各 feature 的 REST 接口在线读写。
package replica

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/tag"
	"github.com/squyrrl/api/internal/infra/db"
)

const (
	// DefaultPullLimit / MaxPullLimit 是每次拉取每类实体的条数上限。
	DefaultPullLimit = 200
	MaxPullLimit     = 500
	// MaxMutations 是一次推送最多带多少条改动。
	MaxMutations = 100
)

type Service struct {
	pool     *pgxpool.Pool
	quota    *quota.Service
	snippets *snippet.Service
	pages    *page.Service
	tags     *tag.Repo
}

func NewService(pool *pgxpool.Pool, q *quota.Service, snippets *snippet.Service, pages *page.Service, tags *tag.Repo) *Service {
	return &Service{pool: pool, quota: q, snippets: snippets, pages: pages, tags: tags}
}

// PullOutput 是一页增量。每一项都是那个实体此刻的完整状态（或删除标记），
// 实体之间互不依赖，客户端按任意顺序应用都得到同一个结果。
type PullOutput struct {
	Pages    []page.Page       `json:"pages"`
	Tags     []tag.Tag         `json:"tags"`
	Snippets []snippet.Snippet `json:"snippets"`
	Deleted  Deleted           `json:"deleted"`
	// Cursor 是这一页最后一项的号，下次带着它作 since。
	Cursor  int64 `json:"cursor"`
	HasMore bool  `json:"has_more"`
	// ReconcileGen 变了说明服务端不留墓碑地删掉过云端数据，客户端要对一次账，见 Manifest。
	ReconcileGen int64 `json:"reconcile_gen"`
}

type Deleted struct {
	Pages    []uuid.UUID `json:"pages"`
	Tags     []uuid.UUID `json:"tags"`
	Snippets []uuid.UUID `json:"snippets"`
}

// Pull 返回 since 之后的改动。since = 0 是首次同步，不含删除标记。
//
// 三张表与墓碑在同一个 REPEATABLE READ 快照里读：分开读的话，两次查询之间提交的写入
// 会让游标越过一个还没读到的号。同一账号的写入按号的顺序提交（计数器行锁），
// 所以快照里看得见的号没有空洞，游标可以放心推进到这一页的最后一项。
func (s *Service) Pull(ctx context.Context, userID uuid.UUID, since int64, limit int) (*PullOutput, error) {
	if limit <= 0 {
		limit = DefaultPullLimit
	}
	limit = min(limit, MaxPullLimit)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	pages, pagesFull, err := page.Changes(ctx, tx, userID, since, limit)
	if err != nil {
		return nil, err
	}
	tags, tagsFull, err := tag.Changes(ctx, tx, userID, since, limit)
	if err != nil {
		return nil, err
	}
	snippets, snippetsFull, err := snippet.Changes(ctx, tx, userID, since, limit)
	if err != nil {
		return nil, err
	}
	head, gen, err := cursorOf(ctx, tx, userID)
	if err != nil {
		return nil, err
	}

	// 每个来源都按号升序取了前 limit 条，全局最小的 limit 个号必然都在这里面；
	// 合并后截到 limit，截掉的部分下一页会重新读到。
	type item struct {
		seq   int64
		apply func(*PullOutput)
	}
	items := make([]item, 0, len(pages)+len(tags)+len(snippets))
	for _, c := range pages {
		items = append(items, item{c.Seq, func(o *PullOutput) {
			if c.Page == nil {
				o.Deleted.Pages = append(o.Deleted.Pages, c.ID)
			} else {
				o.Pages = append(o.Pages, *c.Page)
			}
		}})
	}
	for _, c := range tags {
		items = append(items, item{c.Seq, func(o *PullOutput) {
			if c.Tag == nil {
				o.Deleted.Tags = append(o.Deleted.Tags, c.ID)
			} else {
				o.Tags = append(o.Tags, *c.Tag)
			}
		}})
	}
	for _, c := range snippets {
		items = append(items, item{c.Seq, func(o *PullOutput) {
			if c.Snippet == nil {
				o.Deleted.Snippets = append(o.Deleted.Snippets, c.ID)
			} else {
				o.Snippets = append(o.Snippets, *c.Snippet)
			}
		}})
	}
	slices.SortFunc(items, func(a, b item) int { return cmp.Compare(a.seq, b.seq) })

	out := &PullOutput{
		Pages: []page.Page{}, Tags: []tag.Tag{}, Snippets: []snippet.Snippet{},
		Deleted:      Deleted{Pages: []uuid.UUID{}, Tags: []uuid.UUID{}, Snippets: []uuid.UUID{}},
		Cursor:       since,
		HasMore:      pagesFull || tagsFull || snippetsFull || len(items) > limit,
		ReconcileGen: gen,
	}
	if len(items) > limit {
		items = items[:limit]
	}
	for _, it := range items {
		it.apply(out)
		out.Cursor = it.seq
	}
	// 拉完了就把游标推到账号的最新号：没下发的那些号（首次同步略过的删除、
	// 不留墓碑的清理）不必让客户端下次再扫一遍。计数器与数据在同一个快照里，
	// 已提交的计数器值就是看得见的最大号。
	if !out.HasMore {
		out.Cursor = max(out.Cursor, head)
	}
	return out, nil
}

// ManifestOutput 是服务端现存碎片的 id 清单，供客户端对账。
type ManifestOutput struct {
	SnippetIDs []uuid.UUID `json:"snippet_ids"`
	// Cursor 是取清单那一刻账号的最新号。客户端先拉到不小于它，再拿清单比对。
	Cursor       int64 `json:"cursor"`
	ReconcileGen int64 `json:"reconcile_gen"`
}

// Manifest 在 reconcile_gen 变化后使用：本机标着「已同步」、清单里却没有的碎片，
// 是服务端只删了云端副本（降级后清理）的那些，客户端把它们改回未同步。
// 误判成「没有」的代价只是一次重新上传，而服务端按内容相同直接认成成功。
func (s *Service) Manifest(ctx context.Context, userID uuid.UUID) (*ManifestOutput, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	ids, err := snippet.LiveIDs(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	out := &ManifestOutput{SnippetIDs: ids}
	if out.SnippetIDs == nil {
		out.SnippetIDs = []uuid.UUID{}
	}
	if out.Cursor, out.ReconcileGen, err = cursorOf(ctx, tx, userID); err != nil {
		return nil, err
	}
	return out, nil
}

// cursorOf 读账号的最新号与对账代号；从没写过东西的账号两者都是 0。
func cursorOf(ctx context.Context, q db.Querier, userID uuid.UUID) (seq, gen int64, err error) {
	err = q.QueryRow(ctx,
		`SELECT seq, reconcile_gen FROM sync_cursors WHERE user_id = $1`, userID).Scan(&seq, &gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return seq, gen, err
}

// Push 依次应用一批改动，每条一个结果。
//
// 免费档整批拒绝（402 sync_required）：免费档的数据不上行（ADR-075 ⑧），
// 客户端本来就不该推；这一层是防御。
func (s *Service) Push(ctx context.Context, userID uuid.UUID, muts []Mutation) ([]Result, error) {
	if err := s.quota.RequireSync(ctx, userID); err != nil {
		return nil, err
	}
	out := make([]Result, len(muts))
	for i := range muts {
		out[i] = s.apply(ctx, userID, &muts[i])
	}
	return out, nil
}

func (s *Service) apply(ctx context.Context, userID uuid.UUID, m *Mutation) Result {
	if err := m.validate(); err != nil {
		return Result{Status: StatusRejected, Code: CodeInvalid, Error: err.Error()}
	}
	switch m.Entity {
	case EntitySnippet:
		var out *snippet.Outcome
		var err error
		switch m.Op {
		case OpPut:
			out, err = s.snippets.Put(ctx, userID, m.ID, m.BaseVersion, m.Snippet)
		case OpDelete:
			out, err = s.snippets.DeleteSync(ctx, userID, m.ID, m.BaseVersion)
		case OpMove:
			out, err = s.snippets.Move(ctx, userID, m.ID, m.PageID)
		case OpTags:
			out, err = s.snippets.ChangeTags(ctx, userID, m.ID, m.Add, m.Remove)
		}
		if err != nil {
			return failure(m, err)
		}
		r := Result{Status: StatusOK, Snippet: out.Snippet, Deleted: out.Deleted, LoserID: out.LoserID}
		if out.Conflict {
			r.Status = StatusConflict
		}
		return r
	case EntityPage:
		var out *page.Outcome
		var err error
		if m.Op == OpPut {
			out, err = s.pages.Put(ctx, userID, m.ID, m.Page)
		} else {
			out, err = s.pages.DeleteSync(ctx, userID, m.ID)
		}
		if err != nil {
			return failure(m, err)
		}
		return Result{Status: StatusOK, Page: out.Page, Deleted: out.Deleted}
	default: // EntityTag，validate 已排除其它取值
		var out *tag.Outcome
		var err error
		if m.Op == OpPut {
			out, err = s.tags.Put(ctx, userID, m.ID, m.Tag, s.quota.CheckTagCreate)
		} else {
			out, err = s.tags.DeleteSync(ctx, userID, m.ID)
		}
		if err != nil {
			return failure(m, err)
		}
		return Result{Status: StatusOK, Tag: out.Tag, Deleted: out.Deleted}
	}
}

// failure 把错误分成两类：客户端改了也没用、要等用户处理的（rejected），
// 与过一会儿再试就可能成功的（retry）。
func failure(m *Mutation, err error) Result {
	if qe, ok := quota.IsQuotaError(err); ok {
		return Result{Status: StatusRejected, Code: CodeQuota, Error: qe.Msg, Quota: qe}
	}
	switch {
	case errors.Is(err, snippet.ErrIDTaken), errors.Is(err, page.ErrIDTaken), errors.Is(err, tag.ErrIDTaken):
		return Result{Status: StatusRejected, Code: CodeIDTaken, Error: err.Error()}
	case errors.Is(err, snippet.ErrFileNotFound):
		return Result{Status: StatusRejected, Code: CodeFileMissing, Error: err.Error()}
	case errors.Is(err, snippet.ErrTypeChange):
		return Result{Status: StatusRejected, Code: CodeInvalid, Error: err.Error()}
	case errors.Is(err, page.ErrSystemReadOnly):
		return Result{Status: StatusRejected, Code: CodeReadOnly, Error: err.Error()}
	case errors.Is(err, snippet.ErrNotYetCreated):
		// 新建还没推上来：保留这条，等新建成功后再推
		return Result{Status: StatusRetry, Error: err.Error()}
	}
	slog.Warn("同步推送失败", "entity", m.Entity, "op", m.Op, "id", m.ID, "err", err)
	return Result{Status: StatusRetry, Error: "server error"}
}
