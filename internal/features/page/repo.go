package page

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/infra/db"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const pageCols = "id, name, is_hidden, is_system, order_idx, created_at, updated_at"

func scanPage(row pgx.Row) (*Page, error) {
	var p Page
	err := row.Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// Create 插入一条页面记录；client 可通过 id 指定 UUID
func (r *Repo) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Page, error) {
	q := `
		INSERT INTO pages (id, user_id, name, is_hidden, order_idx, created_at)
		VALUES (COALESCE($1, gen_random_uuid()), $2, $3, $4, $5, COALESCE($6, now()))
		RETURNING ` + pageCols
	return scanPage(r.pool.QueryRow(ctx, q, in.ID, userID, in.Name, in.IsHidden, in.OrderIdx, in.CreatedAt))
}

// Get 拉单条；软删除视为不存在
func (r *Repo) Get(ctx context.Context, userID, id uuid.UUID) (*Page, error) {
	var p Page
	err := r.pool.QueryRow(ctx, `
		SELECT `+pageCols+` FROM pages
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID,
	).Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// List 拉用户全部活跃页面，按 order_idx ASC, created_at ASC
func (r *Repo) List(ctx context.Context, userID uuid.UUID) ([]Page, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+pageCols+` FROM pages
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY order_idx ASC, created_at ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Page
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update 局部更新；返回更新后的页面
// 系统页面（is_system=true）禁止改名/隐藏，但允许调整 order_idx
func (r *Repo) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Page, error) {
	cur, err := r.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if cur.IsSystem && (in.Name != nil || in.IsHidden != nil) {
		return nil, ErrSystemReadOnly
	}

	name := cur.Name
	hidden := cur.IsHidden
	order := cur.OrderIdx
	if in.Name != nil {
		name = *in.Name
	}
	if in.IsHidden != nil {
		hidden = *in.IsHidden
	}
	if in.OrderIdx != nil {
		order = *in.OrderIdx
	}

	var p Page
	err = r.pool.QueryRow(ctx, `
		UPDATE pages SET name=$1, is_hidden=$2, order_idx=$3
		WHERE id=$4 AND user_id=$5 AND deleted_at IS NULL
		RETURNING `+pageCols,
		name, hidden, order, id, userID,
	).Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// SoftDelete 软删除；系统页面禁止删除
// 关联的碎片 page_id 被 ON DELETE SET NULL 自动设为 NULL（schema 已配置）
// 但软删除不会触发 FK 级联；这里手动同步把指向该页面的 snippets.page_id 置 NULL。
// 这只是归属变化，不动碎片的版本（ADR-084）；换号由 snippets 上的触发器完成。
func (r *Repo) SoftDelete(ctx context.Context, userID, id uuid.UUID) error {
	cur, err := r.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if cur.IsSystem {
		return ErrSystemReadOnly
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE snippets SET page_id = NULL
		WHERE page_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pages SET deleted_at = now()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// getAny 拉单条，已删除的也返回（deleted 为真）。
func (r *Repo) getAny(ctx context.Context, userID, id uuid.UUID) (p *Page, deleted bool, err error) {
	var deletedAt *time.Time
	var pg Page
	err = r.pool.QueryRow(ctx, `
		SELECT `+pageCols+`, deleted_at FROM pages WHERE id = $1 AND user_id = $2`,
		id, userID,
	).Scan(&pg.ID, &pg.Name, &pg.IsHidden, &pg.IsSystem, &pg.OrderIdx, &pg.CreatedAt, &pg.UpdatedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	return &pg, deletedAt != nil, nil
}

// Change 是增量拉取里的一个页面：活着的带完整内容，删除了的只有 id。
type Change struct {
	Seq  int64
	ID   uuid.UUID
	Page *Page // nil = 已删除
}

// Changes 读 sync_seq > since 的页面改动，按号升序，最多 limit 条；full 表示取满了。
// since = 0 时不下发删除标记。q 必须是调用方开的同一个快照，见 snippet.Changes。
func Changes(ctx context.Context, q db.Querier, userID uuid.UUID, since int64, limit int) ([]Change, bool, error) {
	live := ""
	if since == 0 {
		live = " AND deleted_at IS NULL"
	}
	rows, err := q.Query(ctx, `
		SELECT `+pageCols+`, deleted_at, sync_seq FROM pages
		WHERE user_id = $1 AND sync_seq > $2`+live+`
		ORDER BY sync_seq
		LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var p Page
		var deletedAt *time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.IsHidden, &p.IsSystem, &p.OrderIdx,
			&p.CreatedAt, &p.UpdatedAt, &deletedAt, &p.SyncSeq); err != nil {
			return nil, false, err
		}
		c := Change{Seq: p.SyncSeq, ID: p.ID, Page: &p}
		if deletedAt != nil {
			c.Page = nil
		}
		out = append(out, c)
	}
	return out, len(out) == limit, rows.Err()
}
