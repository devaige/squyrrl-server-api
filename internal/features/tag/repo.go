package tag

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/infra/db"
)

const pgErrUniqueViolation = "23505"

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const tagCols = "id, name, created_at, updated_at"

func scanTag(row pgx.Row) (*Tag, error) {
	var t Tag
	err := row.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	return &t, err
}

// Create 插入一个标签。主键冲突只可能是客户端指定的 id 已被占用 —— 同名不再冲突。
func (r *Repo) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Tag, error) {
	t, err := scanTag(r.pool.QueryRow(ctx, `
		INSERT INTO tags (id, user_id, name, created_at)
		VALUES (COALESCE($1, gen_random_uuid()), $2, $3, COALESCE($4, now()))
		RETURNING `+tagCols,
		in.ID, userID, in.Name, in.CreatedAt))
	if isUniqueViolation(err) {
		return nil, ErrIDTaken
	}
	return t, err
}

// Get 拉单条；软删除视为不存在
func (r *Repo) Get(ctx context.Context, userID, id uuid.UUID) (*Tag, error) {
	t, err := scanTag(r.pool.QueryRow(ctx, `
		SELECT `+tagCols+` FROM tags WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (r *Repo) List(ctx context.Context, userID uuid.UUID) ([]Tag, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+tagCols+` FROM tags WHERE user_id = $1 AND deleted_at IS NULL ORDER BY name ASC, id ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *Repo) Rename(ctx context.Context, userID, id uuid.UUID, name string) (*Tag, error) {
	t, err := scanTag(r.pool.QueryRow(ctx, `
		UPDATE tags SET name = $1 WHERE id = $2 AND user_id = $3 AND deleted_at IS NULL
		RETURNING `+tagCols,
		name, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// Delete 软删除标签，并摘掉它在碎片上的关联（ADR-084）。
//
// 软删而不是物理删：删除本身要作为一次带号的改动被别的设备拉到。
// 摘关联会经由 snippet_tags 上的触发器给受影响的碎片换号，它们的新标签集合随之下发。
func (r *Repo) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE tags SET deleted_at = now()
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM snippet_tags WHERE tag_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Put 把标签写成 in 的样子：没有就新建（先过 checkCreate 这道额度门槛），有就改名
// （ADR-084：以后到达服务端的为准）。已删除的标签不会被一次迟到的改名复活。
func (r *Repo) Put(
	ctx context.Context,
	userID, id uuid.UUID,
	in *PutInput,
	checkCreate func(context.Context, uuid.UUID) error,
) (*Outcome, error) {
	var deletedAt *time.Time
	var cur Tag
	err := r.pool.QueryRow(ctx, `
		SELECT `+tagCols+`, deleted_at FROM tags WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&cur.ID, &cur.Name, &cur.CreatedAt, &cur.UpdatedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := checkCreate(ctx, userID); err != nil {
			return nil, err
		}
		t, err := r.Create(ctx, userID, &CreateInput{ID: &id, Name: in.Name, CreatedAt: in.CreatedAt})
		if err != nil {
			return nil, err
		}
		return &Outcome{Tag: t}, nil
	}
	if err != nil {
		return nil, err
	}
	if deletedAt != nil {
		return &Outcome{Deleted: true}, nil
	}
	if cur.Name == in.Name {
		return &Outcome{Tag: &cur}, nil
	}
	t, err := r.Rename(ctx, userID, id, in.Name)
	if errors.Is(err, ErrNotFound) {
		return &Outcome{Deleted: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Outcome{Tag: t}, nil
}

// DeleteSync 删除标签；已经不在算成功。
func (r *Repo) DeleteSync(ctx context.Context, userID, id uuid.UUID) (*Outcome, error) {
	if err := r.Delete(ctx, userID, id); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return &Outcome{Deleted: true}, nil
}

// Change 是增量拉取里的一个标签：活着的带完整内容，删除了的只有 id。
type Change struct {
	Seq int64
	ID  uuid.UUID
	Tag *Tag // nil = 已删除
}

// Changes 读 sync_seq > since 的标签改动，按号升序，最多 limit 条；full 表示取满了。
// since = 0 时不下发删除标记。q 必须是调用方开的同一个快照，见 snippet.Changes。
func Changes(ctx context.Context, q db.Querier, userID uuid.UUID, since int64, limit int) ([]Change, bool, error) {
	live := ""
	if since == 0 {
		live = " AND deleted_at IS NULL"
	}
	rows, err := q.Query(ctx, `
		SELECT `+tagCols+`, deleted_at, sync_seq FROM tags
		WHERE user_id = $1 AND sync_seq > $2`+live+`
		ORDER BY sync_seq
		LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var t Tag
		var deletedAt *time.Time
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt, &deletedAt, &t.SyncSeq); err != nil {
			return nil, false, err
		}
		c := Change{Seq: t.SyncSeq, ID: t.ID, Tag: &t}
		if deletedAt != nil {
			c.Tag = nil
		}
		out = append(out, c)
	}
	return out, len(out) == limit, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation
}
