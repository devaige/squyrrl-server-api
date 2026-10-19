package snippet

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/squyrrl/api/internal/infra/db"
)

// 本文件是原生客户端增量同步（ADR-084）在碎片这一侧的读写。
//
// 写入按「字段类别」分开：正文类字段整份推送并核对版本，冲突时客户端那份进冲突收件箱；
// 归类（所属页面、标签）不参与版本判定，各自以单独的操作推送 —— 移动以后到达服务端的为准，
// 标签按增减合并。网页端、浏览器扩展这类在线客户端仍走 handler.go 里的 REST 接口。

// Content 是一条碎片的正文类字段，只有它们参与版本判定。
//
// 同步推送整份发送而不是差量：null 就是清空，ADR-023 那条「PATCH 无法置空」的限制在这里不存在。
type Content struct {
	Type        string          `json:"type" binding:"required,oneof=uri url normal special"`
	Subtype     *string         `json:"subtype"`
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	TextContent []byte          `json:"text_content"`
	TextFormat  string          `json:"text_format" binding:"omitempty,oneof=plain markdown code"`
	TextLang    *string         `json:"text_lang"`
	Payload     json.RawMessage `json:"payload"`
	SourceType  string          `json:"source_type" binding:"required,oneof=device upstream"`
	SourceData  json.RawMessage `json:"source_data"`
	Elements    []ElementInput  `json:"elements"`
}

// PutInput 是一次同步写入：正文的完整状态，外加只在新建时使用的字段。
type PutInput struct {
	Content

	// 用户本机时间，原样存储，只用来显示与排序。
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`

	// 新建时的初始归类。碎片已存在时忽略 —— 之后的移动与标签变化各走各的操作。
	PageID *uuid.UUID  `json:"page_id"`
	TagIDs []uuid.UUID `json:"tag_ids"`
}

func (c *Content) normalize() {
	if c.TextFormat == "" {
		c.TextFormat = "plain"
	}
	if len(c.Payload) == 0 || string(c.Payload) == "null" {
		c.Payload = json.RawMessage("{}")
	}
	if len(c.SourceData) == 0 || string(c.SourceData) == "null" {
		c.SourceData = json.RawMessage("{}")
	}
}

// Outcome 是一次同步写入之后，这条碎片在服务端的样子。
type Outcome struct {
	// Snippet 是服务端当前状态；Deleted 时为 nil。
	Snippet *Snippet
	// Deleted 表示这条碎片在服务端已被删除，客户端应在本机删掉它。
	Deleted bool
	// Conflict 表示客户端这次写入没有生效：内容已存进冲突收件箱（LoserID），
	// 或者删除因为别处先改了正文而被拒。
	Conflict bool
	LoserID  *uuid.UUID
}

// ErrNotYetCreated 是对一条服务端从未有过的碎片做移动 / 改标签。
// 那只能是它的新建还没推上来，客户端应保留这个操作稍后重试。
var ErrNotYetCreated = errors.New("snippet does not exist yet")

// Put 把客户端这条碎片的正文写成 in 的样子。base 是客户端改动所基于的版本，0 表示本机新建。
//
// 规则（ADR-084）：
//   - 服务端没有：有墓碑说明是用户删掉的，客户端的内容进冲突收件箱；没有墓碑就新建
//     （降级清理掉的碎片重新上传也走这里）。
//   - 服务端已删：删除先到，内容进冲突收件箱（与已删版本内容相同时不必留副本）。
//   - 内容已经一样：直接成功。丢了响应的重试因此不会凭空造出冲突。
//   - 版本对上：写入，版本 +1。对不上：服务端那份保留，客户端的进冲突收件箱。
//
// 新建的碎片已存在时按 base = 1 处理：id 是客户端生成的，能建出这一行的只有它上一次的尝试。
func (s *Service) Put(ctx context.Context, userID, id uuid.UUID, base int64, in *PutInput) (*Outcome, error) {
	in.normalize()
	cur, err := s.repo.Get(ctx, userID, id)
	if errors.Is(err, ErrNotFound) {
		return s.putMissing(ctx, userID, id, base, in)
	}
	if err != nil {
		return nil, err
	}
	return s.putExisting(ctx, userID, cur, base, in)
}

func (s *Service) putMissing(ctx context.Context, userID, id uuid.UUID, base int64, in *PutInput) (*Outcome, error) {
	tomb, err := s.repo.tombstoned(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if tomb {
		return s.archive(ctx, userID, id, 0, base, &in.Content, &Outcome{Deleted: true})
	}

	if err := s.quota.CheckSnippetCreate(ctx, userID); err != nil {
		return nil, err
	}
	if err := s.repo.EnsureFilesExist(ctx, collectFileIDs(in.Elements)); err != nil {
		return nil, err
	}
	// 引用的页面 / 标签在服务端不存在（别处删了，或它们自己没能同步上来）时照样建碎片、
	// 只是不挂上去：碎片本身比它的归类重要，不能因为归类卡住正文的同步。
	pageID := in.PageID
	if err := s.repo.EnsurePageOwned(ctx, userID, pageID); errors.Is(err, ErrPageNotOwned) {
		pageID = nil
	} else if err != nil {
		return nil, err
	}
	tagIDs, err := s.repo.ownedTags(ctx, userID, in.TagIDs)
	if err != nil {
		return nil, err
	}

	created, err := s.repo.Create(ctx, userID, &CreateInput{
		ID: &id, PageID: pageID, Type: in.Type, Subtype: in.Subtype,
		Title: in.Title, Description: in.Description,
		TextContent: in.TextContent, TextFormat: in.TextFormat, TextLang: in.TextLang,
		Payload: in.Payload, SourceType: in.SourceType, SourceData: in.SourceData,
		TagIDs: tagIDs, Elements: in.Elements,
		CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	})
	if isUniqueViolation(err) {
		// 并发的同一次新建抢先落了库，按已存在处理；不是本账号的就是 id 被占用
		cur, gerr := s.repo.Get(ctx, userID, id)
		if errors.Is(gerr, ErrNotFound) {
			return nil, ErrIDTaken
		}
		if gerr != nil {
			return nil, gerr
		}
		return s.putExisting(ctx, userID, cur, base, in)
	}
	if err != nil {
		return nil, err
	}
	return &Outcome{Snippet: created}, nil
}

func (s *Service) putExisting(ctx context.Context, userID uuid.UUID, cur *Snippet, base int64, in *PutInput) (*Outcome, error) {
	if base == 0 {
		base = 1
	}
	if cur.DeletedAt != nil {
		if sameContent(cur, &in.Content) {
			return &Outcome{Deleted: true}, nil
		}
		return s.archive(ctx, userID, cur.ID, cur.Version, base, &in.Content, &Outcome{Deleted: true})
	}
	if sameContent(cur, &in.Content) {
		return &Outcome{Snippet: cur}, nil
	}
	if st := cur.Restriction(); st != RestrictionNone {
		plan, _ := s.quota.PlanOf(ctx, userID)
		return nil, restrictedError(
			"这条碎片超出当前方案，暂时不可修改；删除仍然可用",
			plan, *cur.RestrictedAt, st)
	}
	if cur.Type != in.Type && in.Type != "normal" {
		return nil, ErrTypeChange
	}
	if err := s.repo.EnsureFilesExist(ctx, collectFileIDs(in.Elements)); err != nil {
		return nil, err
	}

	updated, err := s.repo.applyContent(ctx, userID, cur.ID, base, in)
	switch {
	case err == nil:
		return &Outcome{Snippet: updated}, nil
	case errors.Is(err, ErrVersionConflict):
		latest, gerr := s.repo.Get(ctx, userID, cur.ID)
		if gerr != nil {
			return nil, gerr
		}
		return s.archive(ctx, userID, cur.ID, latest.Version, base, &in.Content, &Outcome{Snippet: latest})
	case errors.Is(err, ErrNotFound):
		// 读完之后、写之前被删了：与「删除先到」同样处理
		return s.archive(ctx, userID, cur.ID, cur.Version, base, &in.Content, &Outcome{Deleted: true})
	default:
		return nil, err
	}
}

// archive 把客户端没能生效的内容存进冲突收件箱，并把 out 标成冲突。
//
// 与 REST 那条路径（archiveConflict）不同，这里失败就报错而不是退化成普通冲突：
// 客户端收到错误会稍后重试，而一旦当成「冲突已处理」，这份内容就没有任何地方留着了。
func (s *Service) archive(
	ctx context.Context,
	userID, originalID uuid.UUID,
	originalVersion, attemptedVersion int64,
	c *Content, out *Outcome,
) (*Outcome, error) {
	inboxID, err := s.repo.EnsureConflictInbox(ctx, userID)
	if err != nil {
		return nil, err
	}
	loserID, err := s.repo.insertLoser(ctx, userID, inboxID, originalID, originalVersion, attemptedVersion, c)
	if err != nil {
		return nil, err
	}
	out.Conflict = true
	out.LoserID = &loserID
	return out, nil
}

// DeleteSync 删除一条碎片。base 是客户端删除时看到的版本。
//
// 已经不在（删过、清理过、从没上来过）一律算成功。版本对不上说明别处先改了正文：
// 删除被拒，碎片带着新内容回到删除它的设备上（ADR-084「改的那份不丢」）。
func (s *Service) DeleteSync(ctx context.Context, userID, id uuid.UUID, base int64) (*Outcome, error) {
	if base == 0 {
		base = 1
	}
	err := s.repo.SoftDelete(ctx, userID, id, base)
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return &Outcome{Deleted: true}, nil
	case errors.Is(err, ErrVersionConflict):
		cur, gerr := s.repo.Get(ctx, userID, id)
		if gerr != nil {
			return nil, gerr
		}
		return &Outcome{Snippet: cur, Conflict: true}, nil
	default:
		return nil, err
	}
}

// Move 把碎片移进 pageID（nil = 移出页面）。以后到达服务端的为准，不核对版本。
//
// 目标页面在服务端已不存在时忽略这次移动，返回当前状态让客户端改回来 ——
// 那个页面的删除稍后也会随拉取到达客户端。
func (s *Service) Move(ctx context.Context, userID, id uuid.UUID, pageID *uuid.UUID) (*Outcome, error) {
	cur, out, err := s.organizable(ctx, userID, id)
	if out != nil || err != nil {
		return out, err
	}
	if err := s.repo.EnsurePageOwned(ctx, userID, pageID); errors.Is(err, ErrPageNotOwned) {
		return &Outcome{Snippet: cur}, nil
	} else if err != nil {
		return nil, err
	}
	if samePage(cur.PageID, pageID) {
		return &Outcome{Snippet: cur}, nil
	}
	if err := s.repo.setPage(ctx, userID, id, pageID); err != nil {
		return nil, err
	}
	return s.current(ctx, userID, id)
}

// ChangeTags 给碎片加上 add、摘掉 remove。按集合合并，两台设备各加一个标签，两个都在。
// 服务端已不存在的标签直接跳过。
func (s *Service) ChangeTags(ctx context.Context, userID, id uuid.UUID, add, remove []uuid.UUID) (*Outcome, error) {
	_, out, err := s.organizable(ctx, userID, id)
	if out != nil || err != nil {
		return out, err
	}
	add, err = s.repo.ownedTags(ctx, userID, add)
	if err != nil {
		return nil, err
	}
	if err := s.repo.changeTags(ctx, id, add, remove); err != nil {
		return nil, err
	}
	return s.current(ctx, userID, id)
}

// organizable 取一条可以调整归类的碎片。返回非 nil 的 Outcome 表示无需再做什么。
func (s *Service) organizable(ctx context.Context, userID, id uuid.UUID) (*Snippet, *Outcome, error) {
	cur, err := s.repo.Get(ctx, userID, id)
	if errors.Is(err, ErrNotFound) {
		tomb, terr := s.repo.tombstoned(ctx, userID, id)
		if terr != nil {
			return nil, nil, terr
		}
		if tomb {
			return nil, &Outcome{Deleted: true}, nil
		}
		return nil, nil, ErrNotYetCreated
	}
	if err != nil {
		return nil, nil, err
	}
	if cur.DeletedAt != nil {
		return nil, &Outcome{Deleted: true}, nil
	}
	if st := cur.Restriction(); st != RestrictionNone {
		plan, _ := s.quota.PlanOf(ctx, userID)
		return nil, nil, restrictedError(
			"这条碎片超出当前方案，暂时不可修改；删除仍然可用",
			plan, *cur.RestrictedAt, st)
	}
	return cur, nil, nil
}

func (s *Service) current(ctx context.Context, userID, id uuid.UUID) (*Outcome, error) {
	cur, err := s.repo.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if cur.DeletedAt != nil {
		return &Outcome{Deleted: true}, nil
	}
	return &Outcome{Snippet: cur}, nil
}

// sameContent 判断服务端当前正文是否已经就是 c。
func sameContent(cur *Snippet, c *Content) bool {
	return cur.Type == c.Type &&
		eqStr(cur.Subtype, c.Subtype) &&
		eqStr(cur.Title, c.Title) &&
		eqStr(cur.Description, c.Description) &&
		bytes.Equal(cur.TextContent, c.TextContent) &&
		cur.TextFormat == c.TextFormat &&
		eqStr(cur.TextLang, c.TextLang) &&
		cur.SourceType == c.SourceType &&
		sameJSON(cur.Payload, c.Payload) &&
		sameJSON(cur.SourceData, c.SourceData) &&
		sameElements(cur.Elements, c.Elements)
}

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func samePage(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameJSON 按语义比较：jsonb 读回来的键序、空白都与客户端发来的不同。
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

func sameElements(cur []Element, in []ElementInput) bool {
	if len(cur) != len(in) {
		return false
	}
	want := slices.Clone(in)
	slices.SortStableFunc(want, func(a, b ElementInput) int { return a.OrderIdx - b.OrderIdx })
	for i, e := range cur {
		w := want[i]
		if e.FileID != w.FileID || e.OrderIdx != w.OrderIdx || !eqStr(e.AltText, w.AltText) {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// =============================================================================
// 仓储
// =============================================================================

func (r *Repo) tombstoned(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM snippet_tombstones WHERE id = $1 AND user_id = $2)`,
		id, userID).Scan(&ok)
	return ok, err
}

// ownedTags 从 ids 里挑出属于该用户、尚未删除的标签，保持原顺序。
func (r *Repo) ownedTags(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM tags WHERE user_id = $1 AND id = ANY($2) AND deleted_at IS NULL`,
		userID, ids)
	if err != nil {
		return nil, err
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(owned))
	for _, id := range ids {
		if slices.Contains(owned, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// applyContent 在版本对上时整份替换正文与附件，版本 +1。
func (r *Repo) applyContent(ctx context.Context, userID, id uuid.UUID, base int64, in *PutInput) (*Snippet, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	cur, err := r.getForUpdate(ctx, tx, userID, id)
	if err != nil {
		return nil, err
	}
	if cur.DeletedAt != nil {
		return nil, ErrNotFound
	}
	if cur.Version != base {
		return nil, ErrVersionConflict
	}

	if _, err := tx.Exec(ctx, `
		UPDATE snippets SET
			type = $1, subtype = $2, title = $3, description = $4,
			text_content = $5, text_format = $6, text_lang = $7, payload = $8,
			source_type = $9, source_data = $10,
			version = version + 1,
			updated_at = COALESCE($11, now())
		WHERE id = $12 AND user_id = $13`,
		in.Type, in.Subtype, in.Title, in.Description,
		in.TextContent, in.TextFormat, in.TextLang, in.Payload,
		in.SourceType, in.SourceData, in.UpdatedAt,
		id, userID); err != nil {
		return nil, err
	}
	if !sameElements(cur.Elements, in.Elements) {
		// element 替换：触发器自动维护 files.ref_count（旧 -1，新 +1）
		if _, err := tx.Exec(ctx, `DELETE FROM elements WHERE snippet_id = $1`, id); err != nil {
			return nil, err
		}
		if len(in.Elements) > 0 {
			if err := insertElements(ctx, tx, id, in.Elements); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID, id)
}

// setPage 只改归属，不动版本与 updated_at（ADR-084）。
func (r *Repo) setPage(ctx context.Context, userID, id uuid.UUID, pageID *uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE snippets SET page_id = $1
		WHERE id = $2 AND user_id = $3 AND deleted_at IS NULL`,
		pageID, id, userID)
	return err
}

// changeTags 增删标签关联。snippet_tags 上的触发器会给碎片换号。
func (r *Repo) changeTags(ctx context.Context, id uuid.UUID, add, remove []uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if len(add) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO snippet_tags (snippet_id, tag_id)
			SELECT $1, unnest($2::uuid[])
			ON CONFLICT DO NOTHING`, id, add); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM snippet_tags WHERE snippet_id = $1 AND tag_id = ANY($2)`,
			id, remove); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Change 是增量拉取里的一条碎片：活着的带完整内容，删除了的只有 id。
type Change struct {
	Seq     int64
	ID      uuid.UUID
	Snippet *Snippet // nil = 已删除
}

// Changes 读 sync_seq > since 的碎片改动（含删除标记与墓碑），按号升序，每个来源最多 limit 条。
// full 表示某个来源取满了 limit 条，后面可能还有。
//
// since = 0 是首次同步：本机什么都没有，删除标记一概不必下发。
// q 必须是调用方开的同一个快照（REPEATABLE READ）——几张表分开读的话，
// 两次查询之间提交的写入会让游标越过还没读到的号。
func Changes(ctx context.Context, q db.Querier, userID uuid.UUID, since int64, limit int) (out []Change, full bool, err error) {
	live := ""
	if since == 0 {
		live = " AND s.deleted_at IS NULL"
	}
	rows, err := q.Query(ctx, `
		SELECT `+snippetCols+`
		FROM snippets s
		WHERE s.user_id = $1 AND s.sync_seq > $2`+live+`
		ORDER BY s.sync_seq
		LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		s, err := scanSnippet(rows)
		if err != nil {
			rows.Close()
			return nil, false, err
		}
		c := Change{Seq: s.SyncSeq, ID: s.ID, Snippet: s}
		if s.DeletedAt != nil {
			c.Snippet = nil
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	full = len(out) == limit
	if since == 0 {
		return out, full, nil
	}

	rows, err = q.Query(ctx, `
		SELECT id, sync_seq FROM snippet_tombstones
		WHERE user_id = $1 AND sync_seq > $2
		ORDER BY sync_seq
		LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, false, err
	}
	n := 0
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.Seq); err != nil {
			rows.Close()
			return nil, false, err
		}
		out = append(out, c)
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	slices.SortFunc(out, func(a, b Change) int { return cmp.Compare(a.Seq, b.Seq) })
	return out, full || n == limit, nil
}

// LiveIDs 是该用户服务端现存（未删除）的全部碎片 id，供客户端对账。
func LiveIDs(ctx context.Context, q db.Querier, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx,
		`SELECT id FROM snippets WHERE user_id = $1 AND deleted_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
