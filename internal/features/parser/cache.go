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

// VersionsPerResource 是同一资源保留的历史版本数上限，超出的由清理循环按「最近抓取」淘汰。
//
// 读路径只取最新一版，所以第 2..N 版**永远不会被下发** —— 它们的价值全在两个
// 消费者身上：强制刷新后的撤销（只需要 2），以及「此内容已更新」的差异展示。
// 取 10 是给后者留的余量。绝大多数资源从不变更，恒为 1 行，这个上界只对被反复
// 编辑的资源生效，所以放大存储的风险远小于它看上去的样子。
const VersionsPerResource = 10

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

// Get 返回该资源**最新**一版的解析结果及其 version；未命中返回 false。
//
// 排序用 created_at（抓取时刻）而不是 version 本身，尽管「按内容的真实先后序」
// 听起来更对。原因是 version 由各 provider 自行定义，形态互不兼容：TG 是 epoch
// 秒、别处可能是 ISO 时间戳或内容哈希，作为 TEXT 比较时 "9" > "10"、哈希则根本
// 无序。用一个在所有 provider 上都成立的序（我们什么时候抓到的），比用一个只在
// 部分 provider 上成立的序要诚实。代价是上游若返回了一个更旧的版本，我们会把它
// 当成最新 —— 那是上游抖动，而不是这里能靠排序修好的问题。
// id 参与兜底排序，保证同一时刻写入的多行有稳定次序（同 restriction.go 的处理）。
func (c *Cache) Get(ctx context.Context, provider, resourceID string) (*ParsedSnippet, string, bool, error) {
	var raw []byte
	var version string
	err := c.pool.QueryRow(ctx, `
		SELECT payload, version FROM parse_cache
		WHERE provider = $1 AND provider_resource_id = $2
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		provider, resourceID).Scan(&raw, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	var snip ParsedSnippet
	if err := json.Unmarshal(raw, &snip); err != nil {
		return nil, "", false, err
	}
	return &snip, version, true, nil
}

// Put 追加一版解析结果。version 为空串表示该 provider 不提供版本信息，
// 此时唯一约束退化成 (provider, resource_id)，行为与改版前一致（每资源恒一行）。
//
// 冲突（同一 version 被重复解析）时 expires_at 取 EXCLUDED，即从现在起重新计时：
// 走到这里说明刚刚有人真的解析了这个资源，它显然还在被使用中 —— 让活跃条目续期、
// 让没人再碰的条目自然老死，正是保留期该有的形状。
//
// payload 则**有条件**覆盖：新结果没有 title 而旧结果有时，保留旧的。
// 同一 version 意味着上游内容逐字未变，那么两次解析本应得到同样的结果；真出现
// 差异只可能是上游这次降级了（限流、部分字段缺失）。而这张表是跨用户共享的，
// 一次降级返回会把一条好缓存换成坏缓存，代价由**所有**后来者承担，且没人会发现。
// 宁可丢弃这次的结果 —— 它按定义不比已有的更新。
func (c *Cache) Put(ctx context.Context, provider, resourceID, version string, snip *ParsedSnippet) error {
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
		INSERT INTO parse_cache (provider, provider_resource_id, version, payload, file_ids, expires_at)
		VALUES ($1, $2, $3, $4, '{}', $5)
		ON CONFLICT (provider, provider_resource_id, version) DO UPDATE
			SET payload = CASE
					WHEN EXCLUDED.payload->>'title' IS NULL
					 AND parse_cache.payload->>'title' IS NOT NULL
					THEN parse_cache.payload
					ELSE EXCLUDED.payload
				END,
			    expires_at = EXCLUDED.expires_at`,
		provider, resourceID, version, raw, expires)
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

// PruneVersions 让每个资源只保留最近 keep 版，多余的删除，返回删除条数。
//
// 与 Sweep 是两件事，不能合并：Sweep 清的是**整条**过期的资源，这里清的是
// 未过期资源里过多的历史版本 —— 一个被频繁编辑的热门资源永远不会过期，却会
// 无上界地累积版本行。
//
// 窗口函数要全表排一次序，和 extapi.PruneLiveSamples 同样的代价与同样的理由：
// 一天一轮，而这是唯一能在「不知道有哪些资源」的前提下按组裁剪的写法。
// 同样带 LIMIT 分批，原因见 Sweep。
func (c *Cache) PruneVersions(ctx context.Context, keep, limit int) (int64, error) {
	tag, err := c.pool.Exec(ctx, `
		DELETE FROM parse_cache
		WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (
					PARTITION BY provider, provider_resource_id
					ORDER BY created_at DESC, id DESC
				) AS rn
				FROM parse_cache
			) ranked
			WHERE rn > $1
			LIMIT $2
		)`, keep, limit)
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

	slog.Info("解析缓存清理启动", "interval", interval, "ttl", c.ttl, "versions_per_resource", VersionsPerResource)
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

// sweepOnce 先清过期行再裁剪版本。顺序有意义：过期清理会顺带带走整个资源的所有
// 版本行，先跑它能让版本裁剪少扫一批注定要消失的数据。
func (c *Cache) sweepOnce(ctx context.Context) {
	if n, err := c.Sweep(ctx, sweepBatch); err != nil {
		slog.Warn("解析缓存清理失败", "err", err)
	} else if n > 0 {
		slog.Info("解析缓存清理完成", "removed", n)
	}

	if n, err := c.PruneVersions(ctx, VersionsPerResource, sweepBatch); err != nil {
		slog.Warn("解析缓存版本裁剪失败", "err", err)
	} else if n > 0 {
		slog.Info("解析缓存版本裁剪完成", "removed", n, "keep", VersionsPerResource)
	}
}
