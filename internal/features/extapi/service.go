package extapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
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
//	跳过：config 缺 url_template（尚未接好）/ 有单价但余额 <= 0
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
		var cfg EndpointConfig
		if err := json.Unmarshal(ep.Config, &cfg); err != nil {
			slog.Warn("extapi endpoint config 解析失败，跳过", "slug", ep.Slug, "err", err)
			continue
		}
		if cfg.URLTemplate == "" {
			continue // 驱动尚未配置，跳过
		}
		if ep.UnitPriceMicros > 0 && ep.BalanceMicros <= 0 {
			slog.Warn("extapi endpoint 余额不足，跳过", "slug", ep.Slug)
			lastErr = fmt.Errorf("endpoint %s 余额不足", ep.Slug)
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
