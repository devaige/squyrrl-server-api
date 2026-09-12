package snippet

import (
	"testing"
	"time"
)

// Retry-After 必须向上取整。向下取整的表现是：客户端在头里说的那一秒重试，
// 又被拒一次 —— 只在小数部分出现，看起来像限流器算错了。
func TestRetryAfterRoundsUp(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	cooldown := time.Hour

	// 上次导出在 59 分 59.4 秒前 ⇒ 还剩 0.6 秒 ⇒ 必须报 1 秒而不是 0。
	last := now.Add(-cooldown + 600*time.Millisecond)
	if got := retryAfter(last, cooldown, now); got != time.Second {
		t.Errorf("剩余 600ms 时应报 1s，实际 %v", got)
	}

	// 剩 10.2 秒 ⇒ 11 秒。
	last = now.Add(-cooldown + 10200*time.Millisecond)
	if got := retryAfter(last, cooldown, now); got != 11*time.Second {
		t.Errorf("剩余 10.2s 时应报 11s，实际 %v", got)
	}
}

// 恒 >= 1 秒：回 0 等于告诉客户端「立刻重试」，正是这一层要防的循环。
// 时钟回拨或行被并发刷新都可能算出负数，不能让它漏出去。
func TestRetryAfterNeverZeroOrNegative(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for _, last := range []time.Time{
		now.Add(-2 * time.Hour), // 早就过期了
		now,                     // 恰好此刻
		now.Add(time.Hour),      // 时钟回拨，未来的时间戳
	} {
		if got := retryAfter(last, time.Hour, now); got < time.Second {
			t.Errorf("last=%v 时返回了 %v，必须 >= 1s", last, got)
		}
	}
}

// 整秒输入不该被多加一秒 —— 那会让每次退避都白等一拍。
func TestRetryAfterExactSecondIsNotInflated(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour + 30*time.Second)
	if got := retryAfter(last, time.Hour, now); got != 30*time.Second {
		t.Errorf("剩余恰好 30s 时应报 30s，实际 %v", got)
	}
}
