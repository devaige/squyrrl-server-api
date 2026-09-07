package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// minOrphanAge 是 ref_count=0 的 file 行在被 GC 回收前享有的豁免期。
//
// 两步上传（ADR-026）的顺序是「先落 file 行，再由碎片创建 element 建立引用」，
// 这中间 ref_count 合法地为 0 —— 它表示「还没人引用」，而不是「已经没人引用」，
// 两种状态在这一列上无法区分。没有年龄下限，一个刚上传完但碎片尚未建好的文件
// 会被当成孤儿删掉，随后碎片创建撞上 elements 的外键约束而失败。
//
// 客户端两步之间通常只隔几秒，但 TG 摄取要串行上传整个相册后才建碎片，
// 窗口能拉到几十分钟，取 1 小时留足余量。代价只是孤儿多占一小时空间。
const minOrphanAge = time.Hour

// GC 定期清理 ref_count=0 且已过豁免期的 file 行 + 对应的对象存储字节。
//
// 顺序：先 DELETE FROM files WHERE ref_count=0 RETURNING storage_key（事务原子，避免被新引用复活），
//
//	再尽力删 S3 对象。S3 删除失败导致的 dust 是可接受 leak（只占空间，不影响功能），
//	监控应在生产环境覆盖此情况。
type GC struct {
	pool     *pgxpool.Pool
	storage  *Client
	interval time.Duration
	minAge   time.Duration
	batch    int
}

func NewGC(pool *pgxpool.Pool, st *Client, interval time.Duration) *GC {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &GC{pool: pool, storage: st, interval: interval, minAge: minOrphanAge, batch: 100}
}

// Run 阻塞执行 GC 循环；调用方放在独立 goroutine
func (g *GC) Run(ctx context.Context) {
	t := time.NewTicker(g.interval)
	defer t.Stop()

	slog.Info("file GC 启动", "interval", g.interval, "min_orphan_age", g.minAge)

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
//
// 年龄比较走 now() 而非 Go 侧算好的时间戳：created_at 由 DEFAULT now() 写入，
// 同一个时钟比同一列才不会被应用服务器的时钟偏移影响。
func (g *GC) sweep(ctx context.Context) (int, error) {
	rows, err := g.pool.Query(ctx, `
		WITH victims AS (
			SELECT id FROM files
			WHERE ref_count = 0
			  AND created_at < now() - make_interval(secs => $2)
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM files WHERE id IN (SELECT id FROM victims)
		RETURNING id, storage_key, thumbnail_key`, g.batch, g.minAge.Seconds())
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
