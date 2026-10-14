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

// 尺寸列 coalesce 到 0：0 就是 Dims 的「未知」，省得每处都扫一组 *int 再判空。
const fileCols = `id, plain_hash, cipher_hash, size_bytes, mime, storage_bucket, storage_key, thumbnail_key, ref_count, created_at,
	coalesce(width, 0), coalesce(height, 0), coalesce(thumb_width, 0), coalesce(thumb_height, 0)`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	var plain, cipher []byte
	var thumb *string
	if err := row.Scan(&f.ID, &plain, &cipher, &f.SizeBytes, &f.Mime,
		&f.StorageBucket, &f.StorageKey, &thumb, &f.RefCount, &f.CreatedAt,
		&f.Width, &f.Height, &f.ThumbWidth, &f.ThumbHeight); err != nil {
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
	dims Dims,
) (*File, error) {
	w, h := dims.nullable()
	row := r.pool.QueryRow(ctx, `
		INSERT INTO files (plain_hash, cipher_hash, size_bytes, mime, storage_bucket, storage_key, width, height)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+fileCols,
		plainHash, cipherHash, sizeBytes, mime, bucket, key, w, h)
	return scanFile(row)
}

// SetThumbnail 在缩略图对象就位后回写 key 与尺寸。
//
// 只在 thumbnail_key 还空着时生效，返回是否真的写了：一行 files 只有一个缩略图槽位，
// 上游补挂（TG 去重命中老文件）与直传收尾理论上能撞在同一行上，先到的为准。
// id 不存在同样返回 false（GC 已删）。
func (r *Repo) SetThumbnail(ctx context.Context, id uuid.UUID, key string, dims Dims) (bool, error) {
	w, h := dims.nullable()
	tag, err := r.pool.Exec(ctx, `
		UPDATE files SET thumbnail_key = $1, thumb_width = $2, thumb_height = $3
		WHERE id = $4 AND thumbnail_key IS NULL`, key, w, h, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
