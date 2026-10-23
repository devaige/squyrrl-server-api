package extapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// maxRespBytes 限制读取的响应体大小。抖音/TikTok 一类 App 接口的单条作品响应
// 动辄几百 KB，1 MiB 会被截断成「不是 JSON」；4 MiB 留足余量，又不至于让个别
// 异常响应拖垮内存。超出视为坏响应（已计费）。
const maxRespBytes = 4 << 20

// defaultTimeout 是单次上游调用的默认超时；config.timeout_ms 可按接口覆盖。
//
// 宁长勿短：实测（2026-10-16）同一个 TikHub 接口这次 2 秒、下次 15 秒以上。
// 而我们这边放弃等待并不会让上游停手 —— 它多半照样处理完、照样扣费，
// 结果却被丢掉，兜底链还要再花一次钱去试下一个。
const defaultTimeout = 30 * time.Second

type Service struct {
	repo    *Repo
	client  *http.Client
	creds   Credentials
	catalog Catalog
}

func NewService(repo *Repo, creds Credentials, catalog Catalog) *Service {
	// 不设全局 timeout：每次调用按接口的 timeout_ms 走 context 单独控制。
	return &Service{repo: repo, client: &http.Client{}, creds: creds, catalog: catalog}
}

// ListForProvider 取某平台的付费兜底链（单价升序）。
func (s *Service) ListForProvider(ctx context.Context, provider string) ([]Endpoint, error) {
	return s.repo.ListForProvider(ctx, provider)
}

// ListEnabled 取全部启用的接口。
func (s *Service) ListEnabled(ctx context.Context) ([]Endpoint, error) {
	return s.repo.ListEnabled(ctx)
}

// Call 向上游发起一次调用：鉴权 → 请求 → 剥外壳 → 计费则记账 → 可选采样。
//
// 不做兜底、不判断内容能不能用 —— 那是 parser 的兜底链和适配器的事。
// 返回值永不为 nil 错误：失败信息在 CallResult.Err 里，与计费信息并存。
func (s *Service) Call(ctx context.Context, ep Endpoint, req Request, resourceID string) CallResult {
	res := CallResult{BalanceAfter: ep.BalanceMicros}

	v, ok := vendors[ep.Vendor]
	if !ok {
		res.Err = &CallError{Kind: KindConfig, Msg: "未登记的上游 " + ep.Vendor}
		return res
	}
	key := s.creds.key(ep.Vendor)
	if key == "" {
		res.Err = &CallError{Kind: KindNoCredential, Msg: "未配置 " + ep.Vendor + " 的 API 密钥"}
		return res
	}
	var cfg EndpointConfig
	if len(ep.Config) > 0 {
		if err := json.Unmarshal(ep.Config, &cfg); err != nil {
			res.Err = &CallError{Kind: KindConfig, Msg: "config 解析失败: " + err.Error()}
			return res
		}
	}
	timeout := defaultTimeout
	if cfg.TimeoutMs > 0 {
		timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		res.Err = &CallError{Kind: KindConfig, Msg: "适配器给出的 URL 无效: " + err.Error()}
		return res
	}
	q := u.Query()
	for k, val := range req.Query {
		q.Set(k, val)
	}
	u.RawQuery = q.Encode()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(cctx, method, u.String(), body)
	if err != nil {
		res.Err = &CallError{Kind: KindConfig, Msg: err.Error()}
		return res
	}
	if len(req.Body) > 0 {
		hreq.Header.Set("Content-Type", "application/json")
	}
	hreq.Header.Set("Accept", "application/json")
	v.authorize(hreq, key)

	start := time.Now()
	resp, err := s.client.Do(hreq)
	if err != nil {
		res.LatencyMs = int(time.Since(start).Milliseconds())
		if errors.Is(err, context.DeadlineExceeded) {
			res.Err = &CallError{Kind: KindTimeout, Msg: fmt.Sprintf("超过 %s 未响应（上游可能仍会计费，对账时留意）", timeout)}
		} else {
			res.Err = &CallError{Kind: KindNetwork, Msg: err.Error()}
		}
		return res
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	resp.Body.Close()
	res.LatencyMs = int(time.Since(start).Milliseconds())
	res.HTTPStatus = resp.StatusCode

	switch {
	case readErr != nil:
		// 状态码已经到手，2xx 的话上游多半已经计费，按它算
		res.Billed = resp.StatusCode >= 200 && resp.StatusCode < 300
		res.Err = &CallError{Kind: KindNetwork, Msg: "读取响应失败: " + readErr.Error()}
	case len(raw) > maxRespBytes:
		res.Billed = resp.StatusCode >= 200 && resp.StatusCode < 300
		res.Err = &CallError{Kind: KindBadResponse, Msg: "响应超过 4 MiB"}
	default:
		res.Data, res.Billed, res.Err = v.unwrap(resp.StatusCode, raw)
	}

	if res.Billed && ep.UnitPriceMicros > 0 {
		res.CostMicros = ep.UnitPriceMicros
		res.BalanceAfter = ep.BalanceMicros - ep.UnitPriceMicros
		epID := ep.ID
		// 记账失败不影响本次结果，但要看得见：账对不上就是从这里开始的
		if err := s.repo.RecordCost(context.WithoutCancel(ctx), ep.AccountID, &epID,
			-ep.UnitPriceMicros, "call", "", resourceID); err != nil {
			slog.Warn("extapi 成本记账失败", "slug", ep.Slug, "err", err)
		}
	}

	if cfg.ArchiveLive {
		res.SampleID = s.archiveLive(context.WithoutCancel(ctx), ep.ID, resourceID, method, u, req, resp.StatusCode, raw, res.LatencyMs)
	}
	return res
}

// archiveLive 存一条线上调用采样。请求快照里没有鉴权头 —— 密钥只在 vendor.authorize
// 里加到 http.Request 上，从不经过这里。
func (s *Service) archiveLive(ctx context.Context, endpointID uuid.UUID, resourceID, method string,
	u *url.URL, req Request, status int, raw []byte, latencyMs int) *uuid.UUID {
	snapReq := map[string]any{"method": method, "url": u.Scheme + "://" + u.Host + u.Path, "query": req.Query}
	if len(req.Body) > 0 {
		snapReq["body"] = jsonOrString(req.Body)
	}
	reqJSON, _ := json.Marshal(snapReq)
	respJSON, _ := json.Marshal(map[string]any{"status": status, "body": jsonOrString(raw)})
	id, err := s.repo.InsertSample(ctx, endpointID, resourceID, reqJSON, respJSON, status, latencyMs)
	if err != nil {
		slog.Warn("extapi 存档失败", "err", err)
		return nil
	}
	return &id
}

// jsonOrString 尽量把内容存成结构化 JSON（便于 JSONB 检索），非 JSON 则退化为字符串。
func jsonOrString(b []byte) any {
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	return string(b)
}

// ---- 管理后台：账户 ----

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.repo.ListAccounts(ctx)
}

func (s *Service) CreateAccount(ctx context.Context, in CreateAccountInput) (*Account, error) {
	if _, ok := vendors[in.Vendor]; !ok {
		return nil, ErrUnknownVendor
	}
	id, err := s.repo.CreateAccount(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAccount(ctx, id)
}

func (s *Service) UpdateAccount(ctx context.Context, id uuid.UUID, in UpdateAccountInput) error {
	return s.repo.UpdateAccount(ctx, id, in)
}

// Topup 录入一笔充值（正数，reason=topup）或手工核减（负数，reason=adjust）。
func (s *Service) Topup(ctx context.Context, accountID uuid.UUID, amountMicros int64, note string) error {
	reason := "topup"
	if amountMicros < 0 {
		reason = "adjust"
	}
	return s.repo.RecordCost(ctx, accountID, nil, amountMicros, reason, note, "")
}

func (s *Service) ListLedger(ctx context.Context, accountID uuid.UUID, includeCalls bool) ([]LedgerEntry, error) {
	return s.repo.ListLedger(ctx, accountID, includeCalls, 200)
}

// ---- 管理后台：接口 ----

func (s *Service) ListEndpoints(ctx context.Context) ([]Endpoint, error) { return s.repo.List(ctx) }

// CreateEndpoint 建一个接口。slug 必须对应已注册的适配器，平台取自适配器，
// 账户必须与适配器是同一个上游 —— 否则建出来也调不通，不如当场拒绝。
func (s *Service) CreateEndpoint(ctx context.Context, in CreateEndpointInput) (*Endpoint, error) {
	info, ok := s.catalog.Adapter(in.Slug)
	if !ok {
		return nil, ErrUnknownAdapter
	}
	acc, err := s.repo.GetAccount(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	if acc.Vendor != info.Vendor {
		return nil, ErrVendorMismatch
	}
	id, err := s.repo.Create(ctx, info.Provider, in)
	if err != nil {
		return nil, err
	}
	return s.repo.Get(ctx, id)
}

func (s *Service) UpdateEndpoint(ctx context.Context, id uuid.UUID, in UpdateEndpointInput) error {
	return s.repo.Update(ctx, id, in)
}

func (s *Service) GetEndpoint(ctx context.Context, id uuid.UUID) (*Endpoint, error) {
	return s.repo.Get(ctx, id)
}

// Adapters 返回代码里注册的全部适配器。
func (s *Service) Adapters() []AdapterInfo { return s.catalog.Adapters() }

// ListSamples 列出接口的线上采样（带上相对示例的结构差异计数）与作为基准的示例（带上示例
// 之间的结构差异）。列表不带请求响应正文（单条可达几百 KB），看详情走 GetSample。
func (s *Service) ListSamples(ctx context.Context, id uuid.UUID) ([]Sample, []Example, error) {
	all, err := s.repo.ListSamples(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var examples []Example
	var watched []string
	if ep, err := s.repo.Get(ctx, id); err == nil {
		examples, watched = s.comparisonFor(ep.Slug)
	}
	bases := baselinesOf(examples)
	compareExamples(examples, bases)
	for i := range all {
		if diffable(all[i]) {
			all[i].Diff = diffAgainst(all[i].Response, bases, watched, false)
		}
		all[i].Request, all[i].Response = nil, nil
	}
	return all, examples, nil
}

// GetSample 返回一条线上采样的完整请求响应，附带结构差异的字段清单。
func (s *Service) GetSample(ctx context.Context, id uuid.UUID) (*Sample, error) {
	x, err := s.repo.GetSample(ctx, id)
	if err != nil {
		return nil, err
	}
	var examples []Example
	var watched []string
	if ep, err := s.repo.Get(ctx, x.EndpointID); err == nil {
		x.EndpointSlug, x.Provider = ep.Slug, ep.Provider
		examples, watched = s.comparisonFor(ep.Slug)
	}
	if diffable(*x) {
		x.Diff = diffAgainst(x.Response, baselinesOf(examples), watched, true)
	}
	return x, nil
}

// MarkSampleViewed 记下后台打开过这条采样的详情，解析记录列表随之不再为它标圆点。
func (s *Service) MarkSampleViewed(ctx context.Context, id uuid.UUID) error {
	return s.repo.MarkSampleViewed(ctx, id)
}

// SampleDrifts 给一批线上采样算结构差异，只返回和示例比出缺失或新增的那些（键为采样 ID）。
// 解析记录列表一页几十次调用：示例按接口只解析一次，采样响应要整份读出来比。
func (s *Service) SampleDrifts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Drift, error) {
	out := map[uuid.UUID]Drift{}
	if len(ids) == 0 {
		return out, nil
	}
	samples, err := s.repo.ListSamplesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	type comparison struct {
		bases   []baseline
		watched []string
	}
	byEndpoint := map[uuid.UUID]*comparison{}
	for _, x := range samples {
		if !diffable(x) {
			continue
		}
		c, ok := byEndpoint[x.EndpointID]
		if !ok {
			c = &comparison{}
			if ep, err := s.repo.Get(ctx, x.EndpointID); err == nil {
				examples, watched := s.comparisonFor(ep.Slug)
				c.bases, c.watched = baselinesOf(examples), watched
			}
			byEndpoint[x.EndpointID] = c
		}
		d := diffAgainst(x.Response, c.bases, c.watched, false)
		if d.MissingCount > 0 || d.AddedCount > 0 {
			out[x.ID] = Drift{MissingCount: d.MissingCount, AddedCount: d.AddedCount, Viewed: x.ViewedAt != nil}
		}
	}
	return out, nil
}

// comparisonFor 取结构比对的两样输入：接口的示例，和适配器代码读取的字段。
func (s *Service) comparisonFor(slug string) ([]Example, []string) {
	if s.catalog == nil {
		return nil, nil
	}
	return s.catalog.Examples(slug), s.catalog.WatchedPaths(slug)
}

// 线上采样的保留规则：最近 7 天、每个接口最多 100 条。
//
// 7 天对应「从解析记录点回当次响应」的排查窗口；100 条封顶让高频接口的存量与调用量无关
// （单条几十到上百 KB）。写成常量而不是环境变量：它们不随部署环境变化。
const (
	LiveSamplesPerEndpoint = 100
	LiveSampleRetention    = 7 * 24 * time.Hour
)

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

	slog.Info("解析 API 采样限量启动", "interval", interval, "keep", LiveSamplesPerEndpoint, "retention", LiveSampleRetention)
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
	n, err := s.repo.PruneLiveSamples(ctx, LiveSamplesPerEndpoint, LiveSampleRetention)
	if err != nil {
		slog.Warn("解析 API 采样限量失败", "err", err)
		return
	}
	if n > 0 {
		slog.Info("解析 API 采样限量完成", "removed", n)
	}
}
