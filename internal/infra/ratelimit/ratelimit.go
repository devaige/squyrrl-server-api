// Package ratelimit 提供一个进程内的固定窗口限流器。
//
// 刻意不引 Redis：本项目已在 ADR-054 把 Redis 整个移除，异步工作一律跑进程内 goroutine，
// 只有真出现跨进程扇出需求才会重新考虑外部组件。api 现在是单实例，进程内计数足够。
// 代价是**重启即清零** —— 攻击者无法主动触发重启，所以这个代价可接受。
// 将来真要多实例，Allow 的签名不变，换掉内部实现即可。
package ratelimit

import (
	"sync"
	"time"
)

// maxKeys 是同时跟踪的键数上限，用来给内存封顶。
//
// 达到上限后**拒绝新键**（而不是清空重来或无限增长）：分布式滥用下宁可误伤，
// 也不能让一个公开端点把进程的内存吃穿。真被打到这一步时，第三层的发信预算才是兜底。
const maxKeys = 100_000

type window struct {
	n       int
	resetAt time.Time
}

type Limiter struct {
	mu     sync.Mutex
	hits   map[string]*window
	limit  int
	period time.Duration
}

// New 构造限流器。limit <= 0 表示不启用，Allow 恒真。
func New(limit int, period time.Duration) *Limiter {
	return &Limiter{hits: make(map[string]*window), limit: limit, period: period}
}

// Allow 记一次命中并报告是否放行。
func (l *Limiter) Allow(key string) bool {
	if l.limit <= 0 {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if w, ok := l.hits[key]; ok {
		if now.After(w.resetAt) {
			w.n, w.resetAt = 1, now.Add(l.period)
			return true
		}
		if w.n >= l.limit {
			return false
		}
		w.n++
		return true
	}

	// 新键。先按量清扫过期项 —— 不起 janitor goroutine 是因为那需要一个要被管理的
	// 生命周期（谁 Stop、测试里怎么等它收敛），而按量清扫没有生命周期，
	// 且清扫成本天然被键的增长速度摊薄。
	if len(l.hits) >= maxKeys {
		l.sweep(now)
		if len(l.hits) >= maxKeys {
			return false
		}
	}
	l.hits[key] = &window{n: 1, resetAt: now.Add(l.period)}
	return true
}

func (l *Limiter) sweep(now time.Time) {
	for k, w := range l.hits {
		if now.After(w.resetAt) {
			delete(l.hits, k)
		}
	}
}
