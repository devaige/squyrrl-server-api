package file

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const intentCols = `id, user_id, plain_hash, cipher_hash, size_bytes, mime,
	storage_key, coalesce(r2_upload_id, ''), part_size, part_count, status, expires_at`

func scanIntent(row pgx.Row) (*UploadIntent, error) {
	var it UploadIntent
	if err := row.Scan(&it.ID, &it.UserID, &it.PlainHash, &it.CipherHash, &it.SizeBytes,
		&it.Mime, &it.StorageKey, &it.R2UploadID, &it.PartSize, &it.PartCount,
		&it.Status, &it.ExpiresAt); err != nil {
		return nil, err
	}
	return &it, nil
}

func (r *Repo) InsertIntent(ctx context.Context, it *UploadIntent) (*UploadIntent, error) {
	var r2 *string
	if it.R2UploadID != "" {
		r2 = &it.R2UploadID
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO upload_intents
			(user_id, plain_hash, cipher_hash, size_bytes, mime, storage_key,
			 r2_upload_id, part_size, part_count, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+intentCols,
		it.UserID, it.PlainHash, it.CipherHash, it.SizeBytes, it.Mime, it.StorageKey,
		r2, it.PartSize, it.PartCount, it.ExpiresAt)
	return scanIntent(row)
}

// GetIntentForUser 按 (id, user_id) 取。带上 user_id 而不是只按 id：
// 意图行里含 storage_key，凭它能 commit 出一条 files 记录，不能让别的用户拿 id 就收尾。
func (r *Repo) GetIntentForUser(ctx context.Context, id, userID uuid.UUID) (*UploadIntent, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+intentCols+` FROM upload_intents WHERE id = $1 AND user_id = $2`, id, userID)
	it, err := scanIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntentNotFound
	}
	return it, err
}

// MarkIntentStatus 把 pending 推进到终态。带 status='pending' 条件是为了幂等：
// 重复 commit 的第二次会影响 0 行，调用方据此拒绝，而不是重复写 files。
func (r *Repo) MarkIntentStatus(ctx context.Context, id uuid.UUID, status string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE upload_intents SET status = $2 WHERE id = $1 AND status = 'pending'`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrIntentState
	}
	return nil
}

// TakeExpiredIntents 领取一批过期未收尾的意图交给 sweeper。
//
// 直接 UPDATE 成 'aborted' 再返回，而不是先读后写：sweeper 可能与下一轮 tick
// 重叠，SKIP LOCKED + 状态翻转让同一行不会被两个 sweep 同时清理。
// R2 侧的 abort/delete 在事务外尽力而为，失败留下的分片是可接受的 dust
// —— 与 file GC 的取舍一致（坏指针比 dust 更糟）。
func (r *Repo) TakeExpiredIntents(ctx context.Context, limit int) ([]*UploadIntent, error) {
	rows, err := r.pool.Query(ctx, `
		WITH victims AS (
			SELECT id FROM upload_intents
			WHERE status = 'pending' AND expires_at < now()
			ORDER BY expires_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE upload_intents SET status = 'aborted'
		WHERE id IN (SELECT id FROM victims)
		RETURNING `+intentCols, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*UploadIntent
	for rows.Next() {
		it, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PurgeSettledIntents 删掉早已收尾的意图行，避免表无限增长。
// 保留一段时间是为了让重复 commit 得到 ErrIntentState 而不是 ErrIntentNotFound。
func (r *Repo) PurgeSettledIntents(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM upload_intents
		WHERE status <> 'pending' AND expires_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
