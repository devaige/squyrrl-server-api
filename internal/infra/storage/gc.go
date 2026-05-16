package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GC 定期清理 ref_count=0 的 file 行 + 对应的对象存储字节。
//
// 顺序：先 DELETE FROM files WHERE ref_count=0 RETURNING storage_key（事务原子，避免被新引用复活），
//      再尽力删 S3 对象。S3 删除失败导致的 dust 是可接受 leak（只占空间，不影响功能），
//      监控应在生产环境覆盖此情况。
type GC struct {
	pool     *pgxpool.Pool
	storage  *Client
	interval time.Duration
	batch    int
}

func NewGC(pool *pgxpool.Pool, st *Client, interval time.Duration) *GC {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &GC{pool: pool, storage: st, interval: interval, batch: 100}
}

// Run 阻塞执行 GC 循环；调用方放在独立 goroutine
func (g *GC) Run(ctx context.Context) {
	t := time.NewTicker(g.interval)
	defer t.Stop()

	slog.Info("file GC 启动", "interval", g.interval)

	// 启动后立即跑一次（不等第一次 tick）
	g.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			slog.Info("file GC 退出")
			return
		case <-t.C:
			g.sweepOnce(ctx)
		}
	}
}

func (g *GC) sweepOnce(ctx context.Context) {
	n, err := g.sweep(ctx)
	if err != nil {
		slog.Warn("file GC sweep 失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("file GC 清理完成", "removed", n)
	}
}

// sweep 单批清理；先一次性 DELETE 拿到所有要清理的 storage_key，再依次删 S3。
// 同时删除 thumbnail_key 对应对象（若有）。
func (g *GC) sweep(ctx context.Context) (int, error) {
	rows, err := g.pool.Query(ctx, `
		WITH victims AS (
			SELECT id FROM files
			WHERE ref_count = 0
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM files WHERE id IN (SELECT id FROM victims)
		RETURNING id, storage_key, thumbnail_key`, g.batch)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type victim struct {
		ID       uuid.UUID
		Key      string
		ThumbKey *string
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.ID, &v.Key, &v.ThumbKey); err != nil {
			return 0, err
		}
		victims = append(victims, v)
	}

	for _, v := range victims {
		if err := g.storage.Delete(ctx, v.Key); err != nil {
			slog.Warn("storage 删除失败（DB 行已删，留下 dust）",
				"file_id", v.ID, "key", v.Key, "err", err)
		}
		if v.ThumbKey != nil && *v.ThumbKey != "" {
			if err := g.storage.Delete(ctx, *v.ThumbKey); err != nil {
				slog.Warn("thumb 删除失败（dust）",
					"file_id", v.ID, "key", *v.ThumbKey, "err", err)
			}
		}
	}
	return len(victims), nil
}
