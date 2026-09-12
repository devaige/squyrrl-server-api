package parser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cache 封装对 parse_cache 表的存取。同 URI 跨用户复用解析结果。
//
// 保留期由 Cache 自己持有而不是由 Service 逐次传入：写入 TTL 和清理过期行
// 是同一个策略的两半，分给两个类型持有，迟早会出现「写 30 天、清 7 天」
// 这种谁都说不清楚的组合。
type Cache struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// NewCache 建缓存。ttl <= 0 表示写入的行不设期限（**只应出现在测试里**）——
// 生产恒由 config 给一个正值，见 migration 000019 说明为什么「永不过期」是个陷阱。
func NewCache(pool *pgxpool.Pool, ttl time.Duration) *Cache {
	return &Cache{pool: pool, ttl: ttl}
}

// Get 命中返回 ParsedSnippet（含 type/subtype/title/payload/source_*）；未命中返回 false
func (c *Cache) Get(ctx context.Context, provider, resourceID string) (*ParsedSnippet, bool, error) {
	var raw []byte
	err := c.pool.QueryRow(ctx, `
		SELECT payload FROM parse_cache
		WHERE provider = $1 AND provider_resource_id = $2
		  AND (expires_at IS NULL OR expires_at > now())`,
		provider, resourceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var snip ParsedSnippet
	if err := json.Unmarshal(raw, &snip); err != nil {
		return nil, false, err
	}
	return &snip, true, nil
}

// Put 落库或更新缓存，带上 Cache 的保留期。
//
// 冲突时 expires_at 取 EXCLUDED（即从现在起重新计时）而不是保留旧值：
// 走到这里说明刚刚有人真的解析了这个资源，它显然还在被使用中 ——
// 让活跃条目续期、让没人再碰的条目自然老死，正是保留期该有的形状。
func (c *Cache) Put(ctx context.Context, provider, resourceID string, snip *ParsedSnippet) error {
	raw, err := json.Marshal(snip)
	if err != nil {
		return err
	}
	var expires *time.Time
	if c.ttl > 0 {
		t := time.Now().Add(c.ttl)
		expires = &t
	}
	_, err = c.pool.Exec(ctx, `
		INSERT INTO parse_cache (provider, provider_resource_id, payload, file_ids, expires_at)
		VALUES ($1, $2, $3, '{}', $4)
		ON CONFLICT (provider, provider_resource_id) DO UPDATE
			SET payload = EXCLUDED.payload, expires_at = EXCLUDED.expires_at`,
		provider, resourceID, raw, expires)
	return err
}

// Sweep 删除一批已过期的缓存行，返回删除条数。
//
// 带 LIMIT 而不是一条 DELETE 清空：这张表可能很大，而一次长事务会在
// 托管 Postgres 上把 autovacuum 和复制延迟一起拖下水。分批的代价只是
// 积压多的那几轮各跑一次，而清理循环本来就是每天跑的东西。
func (c *Cache) Sweep(ctx context.Context, limit int) (int64, error) {
	tag, err := c.pool.Exec(ctx, `
		DELETE FROM parse_cache
		WHERE id IN (
			SELECT id FROM parse_cache
			WHERE expires_at IS NOT NULL AND expires_at < now()
			ORDER BY expires_at
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// sweepBatch 是单轮清理的批大小。
const sweepBatch = 5000

// RunSweeper 阻塞执行过期缓存的清理循环；调用方放在独立 goroutine。
//
// 与 file GC / 回收站清理同一形状。跑得比它们稀疏得多是因为过期行不占任何
// 正确性成本 —— Get 已经带着 expires_at 条件，多留一天只是多占一天磁盘。
func (c *Cache) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("解析缓存清理启动", "interval", interval, "ttl", c.ttl)
	c.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			slog.Info("解析缓存清理退出")
			return
		case <-t.C:
			c.sweepOnce(ctx)
		}
	}
}

func (c *Cache) sweepOnce(ctx context.Context) {
	n, err := c.Sweep(ctx, sweepBatch)
	if err != nil {
		slog.Warn("解析缓存清理失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("解析缓存清理完成", "removed", n)
	}
}
