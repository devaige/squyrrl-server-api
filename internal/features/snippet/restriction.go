package snippet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/quota"
)

// 降级后超额数据的三个阶段（ADR-075，2026-09-11 用户决策）。
//
// 阶段长度写成常量而不是档位字段：它描述的是「我们给用户多少时间反应」，
// 与他买过哪一档无关 —— 一个 max 用户和一个 basic 用户掉下来之后，
// 需要的反应时间是一样的。
const (
	// RestrictionGrace 期内可读、可删，但不可修改。
	RestrictionGrace = 30 * 24 * time.Hour
	// RestrictionFreeze 期内列表仍可见，但看不了详情。
	RestrictionFreeze = 30 * 24 * time.Hour
)

// Restriction 是一条碎片当前所处的受限阶段，随响应下发给客户端。
type Restriction string

const (
	// RestrictionNone 正常碎片。
	RestrictionNone Restriction = ""
	// RestrictionInGrace 超额但仍在宽限期：能看能删，不能改。
	RestrictionInGrace Restriction = "grace"
	// RestrictionFrozen 冻结：列表里仍然列出，但详情不可读。
	//
	// 「列出但打不开」看起来别扭，却是有意的：把它们直接藏掉，用户会以为数据已经
	// 没了并停止行动；列出来，那一串条目本身就是最有效的续费提醒，
	// 而且用户始终知道自己还有什么没救回来。
	RestrictionFrozen Restriction = "frozen"
)

// StageEnd 返回某个受限阶段的结束时刻，供 402 里的倒计时使用。
func StageEnd(restrictedAt time.Time, stage Restriction) time.Time {
	if stage == RestrictionInGrace {
		return restrictedAt.Add(RestrictionGrace)
	}
	return restrictedAt.Add(RestrictionGrace + RestrictionFreeze)
}

// restrictedError 把受限状态包装成结构化的 402。
//
// 不复用 ErrNotFound 之类的既有错误：那会让界面显示成「这条碎片不存在」，
// 而它明明就在列表里 —— 用户会认为产品坏了，而不是认为自己需要续费。
func restrictedError(msg, plan string, restrictedAt time.Time, stage Restriction) error {
	return quota.ErrDataRestricted(msg, string(stage), StageEnd(restrictedAt, stage), plan)
}

// RestrictionOf 由 restricted_at 推导阶段。
//
// 推导而不是存状态：三个阶段与时间戳之间不可能不一致。若把阶段存成一列，
// 就必须有个任务去推进它，而那个任务停一天，用户看到的阶段就是错的。
func RestrictionOf(restrictedAt *time.Time, now time.Time) Restriction {
	if restrictedAt == nil {
		return RestrictionNone
	}
	switch {
	case now.Before(restrictedAt.Add(RestrictionGrace)):
		return RestrictionInGrace
	case now.Before(restrictedAt.Add(RestrictionGrace + RestrictionFreeze)):
		return RestrictionFrozen
	default:
		// 已过冻结期，等待清理任务删除。在被删掉之前仍按冻结处理，
		// 绝不因为「超时了」就把它放开。
		return RestrictionFrozen
	}
}

// RestrictionSweeper 维护「哪些碎片超出了当前档位的服务端额度」。
//
// 为什么要定期重算而不是降级时标记一次：用户在宽限期内**可以删除**（用户决策），
// 删够了就该重回额度内。一次性标记做不到这件事，而把恢复挂在删除路径上，
// 等于让每一次删除都去数一遍全表。定期重算把这个代价挪到后台，
// 代价是恢复最多延迟一个周期 —— 对一个以天计的流程完全够用。
type RestrictionSweeper struct {
	pool     *pgxpool.Pool
	plans    PlanReader
	interval time.Duration
	batch    int
}

func NewRestrictionSweeper(pool *pgxpool.Pool, plans PlanReader, interval time.Duration) *RestrictionSweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &RestrictionSweeper{pool: pool, plans: plans, interval: interval, batch: 200}
}

func (s *RestrictionSweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()

	slog.Info("超额数据生命周期维护启动", "interval", s.interval)
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			slog.Info("超额数据生命周期维护退出")
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *RestrictionSweeper) sweepOnce(ctx context.Context) {
	marked, cleared, purged, err := s.Sweep(ctx)
	if err != nil {
		slog.Warn("超额数据维护失败", "err", err)
		return
	}
	if marked+cleared+purged > 0 {
		slog.Info("超额数据维护完成", "marked", marked, "cleared", cleared, "purged", purged)
	}
}

// Sweep 跑一轮：重算受限集合，并清理已过冻结期的碎片。
func (s *RestrictionSweeper) Sweep(ctx context.Context) (marked, cleared, purged int, err error) {
	// 先删已过冻结期的 —— 放在重算之前，免得刚标记完又立刻算进统计里。
	purged, err = s.purge(ctx)
	if err != nil {
		return 0, 0, 0, err
	}

	users, err := s.candidates(ctx)
	if err != nil {
		return 0, 0, purged, err
	}
	for _, uid := range users {
		m, c, err := s.reconcileUser(ctx, uid)
		if err != nil {
			slog.Warn("超额数据维护：用户处理失败", "user_id", uid, "err", err)
			continue
		}
		marked += m
		cleared += c
	}
	return marked, cleared, purged, nil
}

// candidates 挑出需要重算的用户：**有云端碎片的**，或**已经有受限标记的**。
//
// 后半句不能省：一个升级回来的用户，他的碎片总数可能仍然很大，
// 但标记必须被清掉 —— 只看「超额的」会漏掉他。
func (s *RestrictionSweeper) candidates(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id FROM snippets
		WHERE deleted_at IS NULL
		GROUP BY user_id
		HAVING count(*) > 0 OR count(restricted_at) > 0
		LIMIT $1`, s.batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// reconcileUser 让某个用户的受限标记与他当前档位一致。
func (s *RestrictionSweeper) reconcileUser(ctx context.Context, userID uuid.UUID) (marked, cleared int, err error) {
	plan, err := s.plans.ActivePlan(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	// ServerSnippets 而不是 Snippets：免费档在服务端的额度是 0，不是 1000。
	allowance := entitlement.MustOf(plan).ServerSnippets()

	// 保留最新的 allowance 条（用户决策 2026-09-11）：近期收集的内容是用户正在用的，
	// 把最新的锁起来会让产品当场变得不可用，而把最旧的锁起来通常无感。
	// id 参与排序是为了让同一毫秒创建的多条有稳定次序 —— 否则两轮 sweep 可能
	// 选中不同的集合，标记会来回抖动。
	const ranked = `
		WITH ranked AS (
			SELECT id, row_number() OVER (ORDER BY created_at DESC, id) AS rn
			FROM snippets WHERE user_id = $1 AND deleted_at IS NULL
		)`

	tag, err := s.pool.Exec(ctx, ranked+`
		UPDATE snippets s SET restricted_at = now()
		FROM ranked r
		WHERE s.id = r.id AND r.rn > $2 AND s.restricted_at IS NULL`,
		userID, allowance)
	if err != nil {
		return 0, 0, fmt.Errorf("标记超额碎片: %w", err)
	}
	marked = int(tag.RowsAffected())

	tag, err = s.pool.Exec(ctx, ranked+`
		UPDATE snippets s SET restricted_at = NULL
		FROM ranked r
		WHERE s.id = r.id AND r.rn <= $2 AND s.restricted_at IS NOT NULL`,
		userID, allowance)
	if err != nil {
		return marked, 0, fmt.Errorf("解除受限标记: %w", err)
	}
	return marked, int(tag.RowsAffected()), nil
}

// purge 永久删除已过冻结期的碎片。
//
// 与回收站清理（TrashSweeper）是两条独立的链路，删除的也是两类东西：
// 那边删的是用户自己删过、超出保留期的；这边删的是用户没删、但已经不在额度内
// 且两个阶段都走完了的。共用 elements 的 CASCADE 与 ref_count 触发器。
func (s *RestrictionSweeper) purge(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM snippets
		WHERE restricted_at IS NOT NULL
		  AND restricted_at < now() - $1::interval`,
		(RestrictionGrace + RestrictionFreeze).String())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
