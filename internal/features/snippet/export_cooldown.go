package snippet

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClaimExportSlot 原子地占用一次导出名额。
//
// 返回 (true, 0) 表示受理；(false, d) 表示还要等 d 之后再来。
//
// 为什么是「占用」而不是「先查再写」：两个并发请求会各自读到「上次导出是昨天」
// 然后一起放行，而这个端点昂贵到不能靠运气。下面那条 upsert 把判定与写入合并成
// 一条语句 —— 后到的请求在行锁上等一下，重新求值 DO UPDATE 的 WHERE 时看到的
// 已经是抢跑者刚写进去的时间戳，于是拿不到名额、返回零行。
//
// **失败的导出同样消耗名额。** Export 从第一个字节起就在扫表和出网，而失败绝大多数
// 发生在中途（客户端断开、上下文超时）。把名额退回去，等于给「打一半就断开」
// 这个最省事的滥用方式发了一张无限次通行证。代价是一次偶发失败要等一个冷却期，
// 而这条路径的消费者有 30 天宽限期可用（ADR-075），量级上不成问题。
//
// cooldown <= 0 关闭这一层，与 config 里其它几个防滥用旋钮同例。
func (e *Exporter) ClaimExportSlot(
	ctx context.Context,
	userID uuid.UUID,
	cooldown time.Duration,
) (bool, time.Duration, error) {
	if cooldown <= 0 {
		return true, 0, nil
	}

	// 三种情形合并在一条语句里：没有行 → INSERT；有行且够旧 → UPDATE；
	// 有行但太新 → DO UPDATE 的 WHERE 不成立，整条语句返回零行。
	// 第三种不是错误，正是被拒的信号。
	err := e.pool.QueryRow(ctx, `
		INSERT INTO export_claims (user_id, claimed_at) VALUES ($1, now())
		ON CONFLICT (user_id) DO UPDATE SET claimed_at = now()
		WHERE export_claims.claimed_at <= now() - $2::interval
		RETURNING claimed_at`,
		userID, cooldown.String()).Scan(new(time.Time))
	if err == nil {
		return true, 0, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, err
	}

	// 被拒：**重新读一次**当前值来算剩余时间。
	// 不复用上面那条语句自己的快照 —— 并发下它看到的是抢跑者写入之前的旧值，
	// 据此算出的 Retry-After 会短一截，客户端照着重试必然再被拒一次，
	// 表现成「限流器算错了」。
	var last time.Time
	if err := e.pool.QueryRow(ctx,
		`SELECT claimed_at FROM export_claims WHERE user_id = $1`, userID).Scan(&last); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 行在两条语句之间消失了（只可能是账户被删）。保守地要求等满一个周期。
			return false, cooldown, nil
		}
		return false, 0, err
	}
	return false, retryAfter(last, cooldown, time.Now()), nil
}

// retryAfter 算出距离下一次可导出还剩多久。
//
// **向上取整到秒，且恒 >= 1 秒**。向下取整会让客户端在 Retry-After 到点时再试一次、
// 再被拒一次 —— 一个只在小数部分出现、看起来像限流器坏了的 bug；
// 而回 0 等于告诉客户端「立刻重试」，恰好是这一层要防的那个循环。
func retryAfter(last time.Time, cooldown time.Duration, now time.Time) time.Duration {
	d := last.Add(cooldown).Sub(now)
	if d < time.Second {
		return time.Second
	}
	return (d + time.Second - 1) / time.Second * time.Second
}
