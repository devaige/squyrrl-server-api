package tag

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgErrUniqueViolation = "23505"

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const tagCols = "id, name, created_at, updated_at"

func (r *Repo) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Tag, error) {
	var t Tag
	err := r.pool.QueryRow(ctx, `
		INSERT INTO tags (id, user_id, name)
		VALUES (COALESCE($1, gen_random_uuid()), $2, $3)
		RETURNING `+tagCols,
		in.ID, userID, in.Name,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if isUniqueViolation(err) {
		return nil, ErrNameDuplicate
	}
	return &t, err
}

func (r *Repo) Get(ctx context.Context, userID, id uuid.UUID) (*Tag, error) {
	var t Tag
	err := r.pool.QueryRow(ctx, `
		SELECT `+tagCols+` FROM tags WHERE id = $1 AND user_id = $2`,
		id, userID,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (r *Repo) List(ctx context.Context, userID uuid.UUID) ([]Tag, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+tagCols+` FROM tags WHERE user_id = $1 ORDER BY name ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repo) Rename(ctx context.Context, userID, id uuid.UUID, name string) (*Tag, error) {
	var t Tag
	err := r.pool.QueryRow(ctx, `
		UPDATE tags SET name = $1 WHERE id = $2 AND user_id = $3
		RETURNING `+tagCols,
		name, id, userID,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if isUniqueViolation(err) {
		return nil, ErrNameDuplicate
	}
	return &t, err
}

// Delete 物理删除标签；snippet_tags 关联通过 ON DELETE CASCADE 自动清理
func (r *Repo) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM tags WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation
}
