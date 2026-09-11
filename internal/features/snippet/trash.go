package snippet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// PlanReader 提供用户当前生效的档位（含宽限期）。由 wallet.Service 实现。
type PlanReader interface {
	ActivePlan(ctx context.Context, userID uuid.UUID) (string, error)
}

// TrashSweeper 按档位的 TrashDays 硬删过期的软删碎片（ADR-075 ⑯）。
//
// 为什么不写成一条 SQL：那需要在 SQL 里再实现一遍「用户现在算哪一档」，
// 而那段逻辑带着宽限期（ADR-075 ⑭），已经在 wallet.PlanState 里有唯一定义。
// 复制一份到这里，两处迟早会分叉 —— 而分叉的表现是**在宽限期内把用户的
// 回收站清空**，一个没人会想到去测、事后也无法补救的 bug。
// 所以这里接受 N+1：先粗筛出可能过期的用户，再逐个按真实档位裁定。
// N 是「回收站里有超过 30 天旧物的用户数」，而这个循环几小时才跑一次。
type TrashSweeper struct {
	pool     *pgxpool.Pool
	plans    PlanReader
	interval time.Duration
	batch    int
}

func NewTrashSweeper(pool *pgxpool.Pool, plans PlanReader, interval time.Duration) *TrashSweeper {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return &TrashSweeper{pool: pool, plans: plans, interval: interval, batch: 500}
}

// minRetentionDays 是所有**启用回收站**的档位里最短的保留期，用作粗筛阈值。
// 它必须 ≤ 任何一档的 TrashDays，否则粗筛会漏掉本该清理的用户。
func minRetentionDays() int {
	min := 0
	for _, key := range entitlement.Order {
		t, ok := entitlement.Of(key)
		if !ok || t.TrashDays <= 0 {
			continue
		}
		if min == 0 || t.TrashDays < min {
			min = t.TrashDays
		}
	}
	return min
}

func (s *TrashSweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()

	slog.Info("回收站清理启动", "interval", s.interval, "min_retention_days", minRetentionDays())
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			slog.Info("回收站清理退出")
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *TrashSweeper) sweepOnce(ctx context.Context) {
	n, err := s.Sweep(ctx)
	if err != nil {
		slog.Warn("回收站清理失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("回收站清理完成", "removed", n)
	}
}

// Sweep 跑一轮清理，返回硬删的碎片数。
//
// 硬删而非再软删一次：elements 对 snippets 是 ON DELETE CASCADE，
// 而 element 的删除触发器会把 files.ref_count 减回去，随后既有的 file GC
// 在孤儿豁免期之后回收字节。整条回收链路已经存在，这里只需要删掉行。
func (s *TrashSweeper) Sweep(ctx context.Context) (int, error) {
	minDays := minRetentionDays()
	if minDays <= 0 {
		return 0, nil // 没有任何档位启用回收站，无事可做
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT user_id FROM snippets
		WHERE deleted_at IS NOT NULL AND deleted_at < now() - make_interval(days => $1)
		LIMIT $2`,
		minDays, s.batch)
	if err != nil {
		return 0, err
	}
	users := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		users = append(users, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, uid := range users {
		n, err := s.sweepUser(ctx, uid)
		if err != nil {
			// 单个用户失败不该让整轮停下 —— 下一轮会再试。
			slog.Warn("回收站清理：用户处理失败", "user_id", uid, "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

func (s *TrashSweeper) sweepUser(ctx context.Context, userID uuid.UUID) (int, error) {
	plan, err := s.plans.ActivePlan(ctx, userID)
	if err != nil {
		return 0, err
	}
	t := entitlement.MustOf(plan)

	// TrashDays == 0 的档位（免费档）**一律跳过**，而不是理解成「立即删除」。
	//
	// 免费档本不该有服务端数据，唯一会落到这里的是降级用户 —— 对他们来说，
	// 「订阅到期」与「永久销毁已删内容」之间不该只隔一次定时任务。
	// 这是本代码库里最不可逆的一个动作，不适合由一次计费状态变化顺手触发。
	// 降级账户的数据处置是一条独立的产品决策，做之前先有明确的通知链路。
	if t.TrashDays <= 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM snippets
		WHERE user_id = $1
		  AND deleted_at IS NOT NULL
		  AND deleted_at < now() - make_interval(days => $2)`,
		userID, t.TrashDays)
	if err != nil {
		return 0, fmt.Errorf("删除过期回收站条目: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
