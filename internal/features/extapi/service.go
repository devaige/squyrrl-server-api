package extapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/pricing"
)

type Service struct {
	repo   *Repo
	client *http.Client
}

func NewService(repo *Repo) *Service {
	// 不设全局 timeout：每次调用按 config.timeout_ms 走 context 单独控制，
	// 不同 endpoint 的耗时特征差异很大，全局超时会误伤慢但正常的服务。
	return &Service{repo: repo, client: &http.Client{}}
}

// Fetch 按 provider 的兜底链依次尝试 endpoint：
//
//	跳过：见 usable —— 与 Quote 共用同一个判据
//	失败：网络错 / 非 2xx / 响应非 JSON —— 记录并尝试下一个
//	成功：记一笔平台成本（= 单价）+ 可选采样存档，立即返回
//
// 返回 ErrNoEndpoint 表示「没有任何可尝试的 endpoint」，上层据此回退到内置免费实现。
// 全链尝试后仍失败则返回聚合错误（上层同样可回退）。
func (s *Service) Fetch(ctx context.Context, provider, uri, resourceID string) (*FetchResult, error) {
	eps, err := s.repo.ListForProvider(ctx, provider)
	if err != nil {
		return nil, err
	}

	var lastErr error
	tried := 0
	for _, ep := range eps {
		cfg, _, skip := usable(ep)
		if skip != skipNone {
			// 未配驱动是常态（种子行就是这样落库的），不值得每次解析都刷一条日志；
			// 其余三种都是「配好了却用不了」，要看得见。
			if skip != skipNoDriver {
				slog.Warn("extapi endpoint 跳过", "slug", ep.Slug, "reason", string(skip))
			}
			continue
		}

		tried++
		res, trace, callErr := s.callEndpoint(ctx, ep, cfg, uri, resourceID)
		if callErr != nil {
			lastErr = callErr
			slog.Warn("extapi endpoint 调用失败，尝试下一个", "slug", ep.Slug, "err", callErr)
			if cfg.ArchiveLive {
				s.archiveLive(ctx, ep.ID, resourceID, trace)
			}
			continue
		}

		// 成功：只在此刻扣费，杜绝「失败也计费」；余额是软预算，允许并发下的轻微超支
		if ep.UnitPriceMicros > 0 {
			if err := s.repo.RecordCost(ctx, ep.ID, -ep.UnitPriceMicros, "call", resourceID); err != nil {
				slog.Warn("extapi 成本记账失败", "slug", ep.Slug, "err", err)
			}
		}
		if cfg.ArchiveLive {
			s.archiveLive(ctx, ep.ID, resourceID, trace)
		}
		res.EndpointID = ep.ID
		res.EndpointSlug = ep.Slug
		return res, nil
	}

	if tried == 0 {
		return nil, ErrNoEndpoint
	}
	return nil, fmt.Errorf("provider %s 所有 endpoint 均失败: %w", provider, lastErr)
}

// skipReason 说明一个 endpoint 为什么不参与本次兜底链。
type skipReason string

const (
	skipNone      skipReason = ""
	skipBadConfig skipReason = "config 解析失败"
	skipNoDriver  skipReason = "未配置 url_template"
	skipNoBalance skipReason = "余额不足"
	skipNoPricing skipReason = "加价倍率非法，无法定价"
)

// usable 判定一个 endpoint 此刻能不能被调用，并顺带算出它对应的用户侧售价。
//
// Fetch 与 Quote 共用这一个判据，而且判据里**包含「能不能定价」** ——
// 不变式是：一个 endpoint 可调用 ⟺ 它可报价。
//
// 把定价塞进可用性判断看着别扭，但两者分开写的失败模式都是静默的：
// 报了价却没有 endpoint 可调，用户白花一次钱；调得到却报不出价，
// 一次真实的上游支出对应零收费。后者正是 migration 000015 写下、
// 而代码直到今天才真正堵上的那个洞，所以它不该再有第二次分叉的机会。
//
// margin_bp 库里有 CHECK 兜着，但那拦不住直接改表或者迁移之前的历史行；
// 拿不出价格时宁可整条链退回内置免费实现，也不要调一个收不上钱的上游。
func usable(ep Endpoint) (EndpointConfig, int64, skipReason) {
	var cfg EndpointConfig
	if err := json.Unmarshal(ep.Config, &cfg); err != nil {
		return cfg, 0, skipBadConfig
	}
	if cfg.URLTemplate == "" {
		return cfg, 0, skipNoDriver
	}
	if ep.UnitPriceMicros > 0 && ep.BalanceMicros <= 0 {
		return cfg, 0, skipNoBalance
	}
	cost, err := pricing.CreditCost(ep.UnitPriceMicros, ep.MarginBP)
	if err != nil {
		return cfg, 0, skipNoPricing
	}
	return cfg, cost, skipNone
}

// Quote 报出解析一次该 provider 应向用户收取的 credits。
//
// 取候选链上的**最高价**，而不是「会被第一个试到的那个」。两个理由：
//
//  1. 兜底链的实际落点由运行时失败决定，报价时无从预知。按第一个报价却回退到
//     更贵的那个，就是一次真实上游支出对应一笔偏低的收费 —— 亏损随用量线性
//     放大，且两本账（api_cost_ledger 与 credits_ledger）从不对账，看不出来。
//  2. 计费发生在查缓存之前（ADR-046 的 2026-07-05 反转），价格因此必须是
//     provider 的函数而不是某一次调用的函数；否则同一个链接两次解析可能不同价，
//     而「按请求计价、可预期」正是那次反转要换来的东西。
//
// 代价是链路降级到便宜 endpoint 时会多收 —— 与 pricing.CreditCost 的向上取整
// 同一个取向：宁可多收一个代币，也不要在每次调用上少收。
//
// 返回 ErrNoEndpoint 表示没有可用的付费 endpoint，调用方据此按内置免费实现计价。
func (s *Service) Quote(ctx context.Context, provider string) (int64, error) {
	eps, err := s.repo.ListForProvider(ctx, provider)
	if err != nil {
		return 0, err
	}
	return quoteOf(eps)
}

// quoteOf 是 Quote 的纯函数内核，抽出来是为了能不带数据库地覆盖定价边界。
func quoteOf(eps []Endpoint) (int64, error) {
	var top int64
	found := false
	for _, ep := range eps {
		_, cost, skip := usable(ep)
		if skip != skipNone {
			continue
		}
		if cost > top {
			top = cost
		}
		found = true
	}
	if !found {
		return 0, ErrNoEndpoint
	}
	return top, nil
}

func (s *Service) archiveLive(ctx context.Context, endpointID uuid.UUID, resourceID string, tr *callTrace) {
	if tr == nil {
		return
	}
	req, _ := json.Marshal(tr.request)
	resp, _ := json.Marshal(tr.response)
	if err := s.repo.InsertSample(ctx, endpointID, "live", resourceID, req, resp, tr.status, tr.latencyMs, ""); err != nil {
		slog.Warn("extapi 存档失败", "err", err)
	}
}

// ---- 管理后台用 ----

func (s *Service) ListEndpoints(ctx context.Context) ([]Endpoint, error) { return s.repo.List(ctx) }

func (s *Service) CreateEndpoint(ctx context.Context, in CreateEndpointInput) (*Endpoint, error) {
	return s.repo.Create(ctx, in)
}

func (s *Service) UpdateEndpoint(ctx context.Context, id uuid.UUID, in UpdateEndpointInput) error {
	return s.repo.Update(ctx, id, in)
}

// Topup 录入一笔额度（正数）或手工核减（负数），进 api_cost_ledger。
func (s *Service) Topup(ctx context.Context, id uuid.UUID, amountMicros int64, reason string) error {
	if reason == "" {
		reason = "topup"
	}
	return s.repo.RecordCost(ctx, id, amountMicros, reason, "")
}

// AddSample 录入一条管理员提供的请求/响应示例（kind='sample'）。
func (s *Service) AddSample(ctx context.Context, id uuid.UUID, in CreateSampleInput) error {
	return s.repo.InsertSample(ctx, id, "sample", in.ResourceID,
		nilIfEmpty(in.Request), nilIfEmpty(in.Response), in.HTTPStatus, 0, in.Note)
}

func (s *Service) ListSamples(ctx context.Context, id uuid.UUID) ([]Sample, error) {
	return s.repo.ListSamples(ctx, id)
}

// nilIfEmpty 把空 RawMessage 归零为 nil —— 空串写进 JSONB 列会报 invalid json。
func nilIfEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// LiveSamplesPerEndpoint 是每个 endpoint 保留的线上采样条数上限。
//
// 写成常量而不是环境变量：它不随部署环境变化，取值只由「要看几条才够判断
// response_map 对不对」决定，而那个答案是 20 左右，不是需要调的旋钮。
const LiveSamplesPerEndpoint = 20

// RunSampleSweeper 阻塞执行线上采样的限量循环；调用方放在独立 goroutine。
//
// 与解析缓存清理共用一个间隔（两者都是「只增不减的辅助表」这一类），
// 因此没有单独的配置项。
func (s *Service) RunSampleSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("解析 API 采样限量启动", "interval", interval, "keep", LiveSamplesPerEndpoint)
	s.pruneOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			slog.Info("解析 API 采样限量退出")
			return
		case <-t.C:
			s.pruneOnce(ctx)
		}
	}
}

func (s *Service) pruneOnce(ctx context.Context) {
	n, err := s.repo.PruneLiveSamples(ctx, LiveSamplesPerEndpoint)
	if err != nil {
		slog.Warn("解析 API 采样限量失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("解析 API 采样限量完成", "removed", n)
	}
}
