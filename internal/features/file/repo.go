package file

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const fileCols = `id, plain_hash, cipher_hash, size_bytes, mime, storage_bucket, storage_key, thumbnail_key, ref_count, created_at`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	var plain, cipher []byte
	var thumb *string
	if err := row.Scan(&f.ID, &plain, &cipher, &f.SizeBytes, &f.Mime,
		&f.StorageBucket, &f.StorageKey, &thumb, &f.RefCount, &f.CreatedAt); err != nil {
		return nil, err
	}
	f.PlainHash = hex.EncodeToString(plain)
	f.CipherHash = hex.EncodeToString(cipher)
	if thumb != nil {
		f.ThumbnailKey = *thumb
		f.HasThumbnail = true
	}
	return &f, nil
}

func (r *Repo) FindByPlainHash(ctx context.Context, plainHash []byte) (*File, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+fileCols+` FROM files WHERE plain_hash = $1`, plainHash)
	f, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*File, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+fileCols+` FROM files WHERE id = $1`, id)
	f, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (r *Repo) Insert(
	ctx context.Context,
	plainHash, cipherHash []byte,
	sizeBytes int64,
	mime, bucket, key string,
) (*File, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO files (plain_hash, cipher_hash, size_bytes, mime, storage_bucket, storage_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+fileCols,
		plainHash, cipherHash, sizeBytes, mime, bucket, key)
	return scanFile(row)
}

// SetThumbnailKey 在缩略图生成完毕后回写 column；id 不存在静默成功（GC 已删）
func (r *Repo) SetThumbnailKey(ctx context.Context, id uuid.UUID, key string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE files SET thumbnail_key = $1 WHERE id = $2`, key, id)
	return err
}

// EnsureFilesExist 校验给定 ID 全部存在；snippet/element 创建前调用
func (r *Repo) EnsureFilesExist(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	var n int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return err
	}
	if n != len(ids) {
		return ErrNotFound
	}
	return nil
}
