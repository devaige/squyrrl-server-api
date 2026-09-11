package snippet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const snippetCols = `
	id, page_id, type, subtype, title, description,
	text_content, text_format, text_lang, payload,
	source_type, source_data, version, created_at, updated_at, deleted_at, restricted_at,
	COALESCE((SELECT array_agg(tag_id) FROM snippet_tags WHERE snippet_id = s.id), '{}') AS tag_ids,
	COALESCE(
		(SELECT json_agg(jsonb_build_object(
			'id', e.id,
			'file_id', e.file_id,
			'order_idx', e.order_idx,
			'alt_text', e.alt_text,
			'created_at', e.created_at
		) ORDER BY e.order_idx, e.created_at)
		FROM elements e WHERE e.snippet_id = s.id),
		'[]'::json
	) AS elements`

func scanSnippet(row pgx.Row) (*Snippet, error) {
	var s Snippet
	var elementsJSON []byte
	err := row.Scan(
		&s.ID, &s.PageID, &s.Type, &s.Subtype, &s.Title, &s.Description,
		&s.TextContent, &s.TextFormat, &s.TextLang, &s.Payload,
		&s.SourceType, &s.SourceData, &s.Version, &s.CreatedAt, &s.UpdatedAt, &s.DeletedAt,
		&s.RestrictedAt, &s.TagIDs, &elementsJSON,
	)
	if err != nil {
		return nil, err
	}
	if s.Payload == nil {
		s.Payload = json.RawMessage("{}")
	}
	if s.SourceData == nil {
		s.SourceData = json.RawMessage("{}")
	}
	if s.TagIDs == nil {
		s.TagIDs = []uuid.UUID{}
	}
	if len(elementsJSON) > 0 {
		if err := json.Unmarshal(elementsJSON, &s.Elements); err != nil {
			return nil, err
		}
	}
	if s.Elements == nil {
		s.Elements = []Element{}
	}
	return &s, nil
}

// =============================================================================
// 校验：tag/page 是否归属用户
// =============================================================================

// EnsureTagsOwned 校验 tagIDs 全部属于 userID；不属于的视为不存在
func (r *Repo) EnsureTagsOwned(ctx context.Context, userID uuid.UUID, tagIDs []uuid.UUID) error {
	if len(tagIDs) == 0 {
		return nil
	}
	var n int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM tags WHERE user_id = $1 AND id = ANY($2)`,
		userID, tagIDs).Scan(&n); err != nil {
		return err
	}
	if n != len(tagIDs) {
		return ErrTagNotOwned
	}
	return nil
}

// EnsurePageOwned 校验 pageID 归属用户；nil 时跳过校验（表示 page_id=NULL）
func (r *Repo) EnsurePageOwned(ctx context.Context, userID uuid.UUID, pageID *uuid.UUID) error {
	if pageID == nil {
		return nil
	}
	var n int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM pages
		WHERE user_id = $1 AND id = $2 AND deleted_at IS NULL`,
		userID, *pageID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrPageNotOwned
	}
	return nil
}

// EnsureConflictInbox 找/建用户的"冲突收件箱"系统页面（is_system=true，不可删）
// 仅在第一次出现冲突时按需创建，普通用户看不到这个页面
func (r *Repo) EnsureConflictInbox(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM pages
		WHERE user_id = $1 AND is_system = TRUE AND name = '冲突收件箱' AND deleted_at IS NULL`,
		userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = r.pool.QueryRow(ctx, `
		INSERT INTO pages (user_id, name, is_system, order_idx)
		VALUES ($1, '冲突收件箱', TRUE, 9999)
		RETURNING id`,
		userID).Scan(&id)
	return id, err
}

// InsertConflictLoser 把客户端尝试写入但版本冲突的内容（loser）作为新 snippet 入库
// 字段从 cur + in 合并：客户端 in 字段存在则用 in，否则用 cur 的值（保留意图全貌）
// 副本固定写到 conflict inbox 页面，source_data 标记其来源
func (r *Repo) InsertConflictLoser(
	ctx context.Context,
	userID, pageID uuid.UUID,
	cur *Snippet,
	in *UpdateInput,
) (uuid.UUID, error) {
	title := cur.Title
	if in.Title != nil {
		title = in.Title
	}
	description := cur.Description
	if in.Description != nil {
		description = in.Description
	}
	textContent := cur.TextContent
	if in.TextContent != nil {
		textContent = in.TextContent
	}
	textFormat := cur.TextFormat
	if in.TextFormat != nil {
		textFormat = *in.TextFormat
	}
	textLang := cur.TextLang
	if in.TextLang != nil {
		textLang = in.TextLang
	}
	payload := cur.Payload
	if len(in.Payload) > 0 {
		payload = in.Payload
	}

	sourceData, _ := json.Marshal(map[string]any{
		"provider":                     "conflict",
		"original_snippet_id":          cur.ID.String(),
		"original_version_at_conflict": cur.Version,
		"client_attempted_version":     in.Version,
	})

	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO snippets (
			user_id, page_id, type, subtype, title, description,
			text_content, text_format, text_lang, payload,
			source_type, source_data
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		userID, pageID, cur.Type, cur.Subtype,
		title, description, textContent, textFormat, textLang, payload,
		"device", sourceData).Scan(&id)
	return id, err
}

// EnsureFilesExist 校验所有 fileIDs 都存在（files 是全局共享去重表，不属于具体用户）
func (r *Repo) EnsureFilesExist(ctx context.Context, fileIDs []uuid.UUID) error {
	if len(fileIDs) == 0 {
		return nil
	}
	// 去重以避免重复元素夸大计数
	seen := make(map[uuid.UUID]struct{}, len(fileIDs))
	uniq := make([]uuid.UUID, 0, len(fileIDs))
	for _, id := range fileIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	var n int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE id = ANY($1)`, uniq).Scan(&n); err != nil {
		return err
	}
	if n != len(uniq) {
		return ErrFileNotFound
	}
	return nil
}

// =============================================================================
// CRUD
// =============================================================================

// Create 新建碎片；tag 关联在同一事务内写入
func (r *Repo) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Snippet, error) {
	textFormat := in.TextFormat
	if textFormat == "" {
		textFormat = "plain"
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	sourceData := in.SourceData
	if len(sourceData) == 0 {
		sourceData = json.RawMessage("{}")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO snippets (
			id, user_id, page_id, type, subtype, title, description,
			text_content, text_format, text_lang, payload,
			source_type, source_data
		)
		VALUES (
			COALESCE($1, gen_random_uuid()), $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11,
			$12, $13
		)
		RETURNING id`,
		in.ID, userID, in.PageID, in.Type, in.Subtype, in.Title, in.Description,
		in.TextContent, textFormat, in.TextLang, payload,
		in.SourceType, sourceData,
	).Scan(&id); err != nil {
		return nil, err
	}

	if len(in.TagIDs) > 0 {
		if err := insertSnippetTags(ctx, tx, id, in.TagIDs); err != nil {
			return nil, err
		}
	}
	if len(in.Elements) > 0 {
		if err := insertElements(ctx, tx, id, in.Elements); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID, id)
}

// Get 拉单个碎片
func (r *Repo) Get(ctx context.Context, userID, id uuid.UUID) (*Snippet, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+snippetCols+`
		FROM snippets s
		WHERE s.id = $1 AND s.user_id = $2`,
		id, userID)
	s, err := scanSnippet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// List 按过滤条件返回；若提供了 since，按 updated_at 升序作为同步流；否则按 updated_at 降序作为浏览流
func (r *Repo) List(ctx context.Context, userID uuid.UUID, in *ListInput) ([]Snippet, error) {
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	var conds []string
	args := []any{userID}
	conds = append(conds, "s.user_id = $1")

	if !in.IncludeDeleted {
		conds = append(conds, "s.deleted_at IS NULL")
	}
	if in.PageID != nil {
		args = append(args, *in.PageID)
		conds = append(conds, "s.page_id = $"+itoa(len(args)))
	}
	if in.TagID != nil {
		args = append(args, *in.TagID)
		conds = append(conds, "s.id IN (SELECT snippet_id FROM snippet_tags WHERE tag_id = $"+itoa(len(args))+")")
	}
	if in.Q != nil && strings.TrimSpace(*in.Q) != "" {
		q := "%" + strings.TrimSpace(*in.Q) + "%"
		args = append(args, q)
		// 仅查可见字段：title / description。text_content 是混淆字节，不参与服务端检索。
		conds = append(conds,
			"(COALESCE(s.title, '') ILIKE $"+itoa(len(args))+
				" OR COALESCE(s.description, '') ILIKE $"+itoa(len(args))+")")
	}

	order := "s.updated_at DESC"
	if in.Since != nil {
		args = append(args, *in.Since)
		conds = append(conds, "s.updated_at >= $"+itoa(len(args)))
		order = "s.updated_at ASC"
	}
	args = append(args, limit)

	q := `
		SELECT ` + snippetCols + `
		FROM snippets s
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY ` + order + `, s.id ASC
		LIMIT $` + itoa(len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Snippet
	for rows.Next() {
		s, err := scanSnippet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Update 应用乐观锁；要求 expectedVersion 与当前版本一致
// 字段为 nil 则保持不变；TagIDs 为 nil 不变，空切片清空，非空替换
func (r *Repo) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Snippet, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	cur, err := r.getForUpdate(ctx, tx, userID, id)
	if err != nil {
		return nil, err
	}
	if cur.Version != in.Version {
		return nil, ErrVersionConflict
	}
	if cur.DeletedAt != nil {
		return nil, ErrNotFound
	}

	// 应用字段差异
	// ClearPage 先判：同时传了 page_id 和 clear_page 时，「移出」是更明确的意图 ——
	// 一个想把碎片放进某页的调用方不会顺手带上 clear_page。
	if in.ClearPage {
		cur.PageID = nil
	} else if in.PageID != nil {
		cur.PageID = in.PageID
	}
	if in.Title != nil {
		cur.Title = in.Title
	}
	if in.Description != nil {
		cur.Description = in.Description
	}
	if in.TextContent != nil {
		cur.TextContent = in.TextContent
	}
	if in.TextFormat != nil {
		cur.TextFormat = *in.TextFormat
	}
	if in.TextLang != nil {
		cur.TextLang = in.TextLang
	}
	if len(in.Payload) > 0 {
		cur.Payload = in.Payload
	}

	if _, err := tx.Exec(ctx, `
		UPDATE snippets SET
			page_id = $1,
			title = $2,
			description = $3,
			text_content = $4,
			text_format = $5,
			text_lang = $6,
			payload = $7,
			version = version + 1
		WHERE id = $8 AND user_id = $9 AND version = $10`,
		cur.PageID, cur.Title, cur.Description, cur.TextContent,
		cur.TextFormat, cur.TextLang, cur.Payload,
		id, userID, in.Version); err != nil {
		return nil, err
	}

	if in.TagIDs != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM snippet_tags WHERE snippet_id = $1`, id); err != nil {
			return nil, err
		}
		if len(*in.TagIDs) > 0 {
			if err := insertSnippetTags(ctx, tx, id, *in.TagIDs); err != nil {
				return nil, err
			}
		}
	}

	// element 替换：触发器自动维护 files.ref_count（旧 -1，新 +1）
	if in.Elements != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM elements WHERE snippet_id = $1`, id); err != nil {
			return nil, err
		}
		if len(*in.Elements) > 0 {
			if err := insertElements(ctx, tx, id, *in.Elements); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, userID, id)
}

// SoftDelete 设置 deleted_at；version 自增；同时删除 elements 行（触发 ref_count -1）
// element 删除写在事务里，保证软删的同时空间引用立刻释放
func (r *Repo) SoftDelete(ctx context.Context, userID, id uuid.UUID, expectedVersion int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE snippets SET deleted_at = now(), version = version + 1
		WHERE id = $1 AND user_id = $2 AND version = $3 AND deleted_at IS NULL`,
		id, userID, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// 区分：不存在 vs 版本冲突 vs 已删
		var exists bool
		_ = tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM snippets WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL)`,
			id, userID).Scan(&exists)
		if !exists {
			return ErrNotFound
		}
		return ErrVersionConflict
	}

	// 释放 element 引用 → 触发器把对应 file.ref_count -1
	if _, err := tx.Exec(ctx, `DELETE FROM elements WHERE snippet_id = $1`, id); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// =============================================================================
// 内部工具
// =============================================================================

func (r *Repo) getForUpdate(ctx context.Context, tx pgx.Tx, userID, id uuid.UUID) (*Snippet, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+snippetCols+`
		FROM snippets s
		WHERE s.id = $1 AND s.user_id = $2
		FOR UPDATE`,
		id, userID)
	s, err := scanSnippet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func insertSnippetTags(ctx context.Context, tx pgx.Tx, snippetID uuid.UUID, tagIDs []uuid.UUID) error {
	rows := make([][]any, 0, len(tagIDs))
	for _, t := range tagIDs {
		rows = append(rows, []any{snippetID, t})
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"snippet_tags"},
		[]string{"snippet_id", "tag_id"},
		pgx.CopyFromRows(rows),
	)
	return err
}

func insertElements(ctx context.Context, tx pgx.Tx, snippetID uuid.UUID, els []ElementInput) error {
	rows := make([][]any, 0, len(els))
	for _, e := range els {
		rows = append(rows, []any{snippetID, e.FileID, e.OrderIdx, e.AltText})
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"elements"},
		[]string{"snippet_id", "file_id", "order_idx", "alt_text"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// itoa 是 strconv.Itoa 的轻量替代，避免引入额外依赖
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
