package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestAllow_WithinAndOverLimit(t *testing.T) {
	l := New(3, time.Minute)
	for i := 1; i <= 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("第 %d 次应放行", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("第 4 次应被拒")
	}
	// 不同键互不影响
	if !l.Allow("5.6.7.8") {
		t.Fatal("另一个键应放行")
	}
}

func TestAllow_WindowRollsOver(t *testing.T) {
	l := New(1, 20*time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("首次应放行")
	}
	if l.Allow("k") {
		t.Fatal("窗口内第二次应被拒")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("窗口滚过后应重新放行")
	}
}

// limit <= 0 表示不启用，必须完全放行——配置里 0 是「关掉这一层」的开关。
func TestAllow_DisabledWhenLimitNotPositive(t *testing.T) {
	l := New(0, time.Minute)
	for i := 0; i < 1000; i++ {
		if !l.Allow("k") {
			t.Fatal("limit=0 时应恒放行")
		}
	}
}

// 过期项必须能被回收，否则长期运行会把内存吃穿。
func TestSweep_ReclaimsExpiredKeys(t *testing.T) {
	l := New(1, time.Millisecond)
	for i := 0; i < 100; i++ {
		l.Allow(string(rune(i)))
	}
	time.Sleep(5 * time.Millisecond)
	l.mu.Lock()
	l.sweep(time.Now())
	n := len(l.hits)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("清扫后应为空，实际 %d", n)
	}
}

func TestAllow_ConcurrentIsRaceFree(t *testing.T) {
	l := New(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.Allow("shared")
			}
		}()
	}
	wg.Wait()
	if l.Allow("shared") {
		t.Fatal("累计 1000 次后应被拒")
	}
}
