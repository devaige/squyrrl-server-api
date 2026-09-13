package parser

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// countingParser 记下上游被真正调用了几次。整组测试问的都是同一个问题：
// 「这次请求有没有花掉一次上游调用」—— 而那正是这批改动要控制的成本。
type countingParser struct {
	calls   atomic.Int32
	version string
	block   chan struct{} // 非 nil 时 Parse 阻塞直到它被关闭，用来制造并发窗口
}

func (p *countingParser) Provider() string    { return "test_svc" }
func (p *countingParser) SnippetType() string { return "special" }

func (p *countingParser) Match(uri string) (string, bool) {
	const prefix = "https://example.test/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	return strings.TrimPrefix(uri, prefix), true
}

func (p *countingParser) Parse(ctx context.Context, uri, resourceID string) (*ParseResult, error) {
	p.calls.Add(1)
	if p.block != nil {
		<-p.block
	}
	title := "上游内容"
	return &ParseResult{
		Title:      &title,
		Payload:    json.RawMessage(`{}`),
		SourceData: json.RawMessage(`{}`),
		Version:    p.version,
	}, nil
}

// newTestService 装配一个只有内置 parser、不计费、不接 extapi 的 Service。
// baseCost=0 让 quote 恒返回 0，于是 wallet 整条路径都不会被触碰（可以传 nil）。
func newTestService(t *testing.T, p Parser, cooldown time.Duration) *Service {
	t.Helper()
	pool := testPool(t)
	cache := newTestCache(t, pool, p.Provider())
	reg := NewRegistry()
	reg.Register(p)
	return NewService(reg, cache, nil, nil, 0, cooldown)
}

// force 跳过缓存重取一次；冷却期内的第二次 force 退回读缓存，不再打上游。
//
// 「退回读缓存」而不是报错是有意的：冷却期内那份缓存正是几秒前刚刷新的，
// 用户要的新鲜内容已经在手上，报错等于把一次成功说成失败。
func TestParseForceRefetchesThenCoolsDown(t *testing.T) {
	p := &countingParser{version: "v1"}
	svc := newTestService(t, p, time.Hour)
	ctx := context.Background()
	uid := uuid.New()
	const uri = "https://example.test/res1"

	// 第一次：缓存空，无论如何都要打上游
	if _, err := svc.Parse(ctx, uid, ParseInput{URI: uri}); err != nil {
		t.Fatalf("首次解析失败: %v", err)
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("首次解析应打 1 次上游，实际 %d", got)
	}

	// 普通请求命中缓存，不打上游
	res, err := svc.Parse(ctx, uid, ParseInput{URI: uri})
	if err != nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	if !res.Cached || p.calls.Load() != 1 {
		t.Fatalf("普通请求应命中缓存：cached=%v calls=%d", res.Cached, p.calls.Load())
	}

	// force：绕过缓存，真打上游
	res, err = svc.Parse(ctx, uid, ParseInput{URI: uri, Force: true})
	if err != nil {
		t.Fatalf("强制刷新失败: %v", err)
	}
	if res.Cached || p.calls.Load() != 2 {
		t.Fatalf("强制刷新应重取：cached=%v calls=%d", res.Cached, p.calls.Load())
	}

	// 冷却期内再 force：退回缓存，不再打上游
	res, err = svc.Parse(ctx, uid, ParseInput{URI: uri, Force: true})
	if err != nil {
		t.Fatalf("冷却期内强制刷新失败: %v", err)
	}
	if !res.Cached {
		t.Fatal("冷却期内的强制刷新应退回读缓存")
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("冷却期内不应再打上游，实际累计 %d 次", got)
	}
}

// 冷却是**按资源**计的：刷了 A 不该把 B 一起挡住。
//
// 反过来说，同一个资源换个用户来刷也照样被挡 —— 那是刻意的，缓存跨用户共享，
// 平台不该为同一份内容连付两次。
func TestParseRefreshCooldownIsPerResource(t *testing.T) {
	p := &countingParser{version: "v1"}
	svc := newTestService(t, p, time.Hour)
	ctx := context.Background()
	uid := uuid.New()

	if _, err := svc.Parse(ctx, uid, ParseInput{URI: "https://example.test/a", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Parse(ctx, uid, ParseInput{URI: "https://example.test/b", Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("两个不同资源各应打一次上游，实际 %d", got)
	}

	// 换个用户刷同一条：仍被冷却挡下
	if _, err := svc.Parse(ctx, uuid.New(), ParseInput{URI: "https://example.test/a", Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("同一资源换用户仍应被冷却挡下，实际 %d", got)
	}
}

// 并发解析同一资源合并成一次上游调用。
//
// 这一条直接对应成本：付费 endpoint 的余额是 soft budget、没有 advisory lock，
// 并发的缓存未命中会各自扣一次上游的钱。单飞是不引入分布式锁时能加的第一层。
func TestParseSingleFlightCollapsesConcurrent(t *testing.T) {
	p := &countingParser{version: "v1", block: make(chan struct{})}
	svc := newTestService(t, p, time.Hour)
	ctx := context.Background()
	uid := uuid.New()
	const uri = "https://example.test/hot"

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Parse(ctx, uid, ParseInput{URI: uri})
		}(i)
	}

	// 等这批请求都进到 Parse 里，再放行上游，确保它们真的重叠在一起。
	// 直接关 channel 的话首个请求可能在其余请求出发前就已经返回，测不到合并。
	time.Sleep(50 * time.Millisecond)
	close(p.block)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求失败: %v", i, err)
		}
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("%d 个并发请求应合并为 1 次上游调用，实际 %d 次", n, got)
	}
}

// provider 不给版本信息时，一切退回改版前的行为：缓存恒一行，force 依然能重取。
func TestParseUnversionedProviderStillRefreshes(t *testing.T) {
	p := &countingParser{version: ""}
	svc := newTestService(t, p, 0) // 冷却关闭
	ctx := context.Background()
	uid := uuid.New()
	const uri = "https://example.test/plain"

	for range 3 {
		if _, err := svc.Parse(ctx, uid, ParseInput{URI: uri, Force: true}); err != nil {
			t.Fatalf("强制刷新失败: %v", err)
		}
	}
	if got := p.calls.Load(); got != 3 {
		t.Fatalf("冷却关闭时每次 force 都应重取，实际 %d 次", got)
	}

	res, err := svc.Parse(ctx, uid, ParseInput{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cached || res.Version != "" {
		t.Fatalf("无版本 provider 应命中缓存且 version 为空：cached=%v version=%q", res.Cached, res.Version)
	}
}
