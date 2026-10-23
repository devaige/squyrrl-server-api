package parser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/pricing"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/ratelimit"
)

// Deps 是 Service 的依赖。Extapi / Prices / Logs 可为 nil（测试里只跑内置来源）。
type Deps struct {
	Registry *Registry
	Cache    *Cache
	Wallet   *wallet.Service
	Extapi   *extapi.Service // nil → 只走内置免费来源
	Adapters AdapterSet
	Prices   *PriceRepo        // nil → 不读覆盖值，全部用自动价
	Logs     *LogRepo          // nil → 不记解析记录
	Resolver ShortLinkResolver // nil → 真实 HTTP 跟跳转

	// DefaultPrice 是只有免费来源的平台的售价（SQUYRRL_PARSE_COST）；<=0 = 解析整体不扣费。
	DefaultPrice    int64
	RefreshCooldown time.Duration
}

type Service struct {
	registry     *Registry
	cache        *Cache
	wallet       *wallet.Service
	extapi       *extapi.Service
	adapters     AdapterSet
	prices       *PriceRepo
	logs         *LogRepo
	resolver     ShortLinkResolver
	defaultPrice int64

	// flight 让并发解析**同一资源**的请求共用一次上游调用。
	//
	// 这不是锦上添花的优化：付费接口的余额是 soft budget，没有 advisory lock，
	// 并发的缓存未命中本来就会小幅超支；而强制刷新是一个用户可以反复按的按钮，
	// 它把「小幅超支」放大成「可被主动触发的循环」。合并同一资源的在途请求，
	// 是在不引入分布式锁的前提下能做的最直接的一层。
	flight singleflight.Group

	// refresh 是强制刷新的冷却窗口，**按资源**而不是按用户计。
	//
	// 按用户计的话，A 刚刷完 B 立刻又能刷同一条，平台为同一份内容付两次钱。
	// 而冷却期内被挡下的请求不报错、退回读缓存 —— 那份缓存恰恰是几秒前刚刷新的，
	// 用户要的「最新内容」已经在手上了，报错反而是把一次成功说成失败。
	refresh *ratelimit.Limiter
}

func NewService(d Deps) *Service {
	// 每个资源每个冷却窗口放行一次真正的重取。冷却期为 0 时走 ratelimit 自己的
	// 「limit<=0 即 Allow 恒真」而不是依赖 period=0 的时间比较 ——
	// 后者要靠两次调用落在不同纳秒上才成立，是个能跑通但说不清楚的关闭方式。
	limit := 1
	if d.RefreshCooldown <= 0 {
		limit = 0
	}
	adapters := d.Adapters
	if adapters == nil {
		adapters = AdapterSet{}
	}
	var resolver ShortLinkResolver = newHTTPResolver()
	if d.Resolver != nil {
		resolver = d.Resolver
	}
	return &Service{
		registry: d.Registry, cache: d.Cache, wallet: d.Wallet,
		extapi: d.Extapi, adapters: adapters, prices: d.Prices, logs: d.Logs,
		resolver: resolver, defaultPrice: d.DefaultPrice,
		refresh: ratelimit.New(limit, d.RefreshCooldown),
	}
}

// Parse 是主入口：选平台 → 定价 → 确认付得起 → 命中缓存或跑兜底链 → 成功才扣费 → 记一笔解析记录。
//
// 计费（ADR-080，取代 ADR-046 的「先扣后取」）：
//   - 价格是平台的固定售价，与实际由哪个来源产出无关，命中缓存也一样；
//   - 先看余额够不够，够才干活；拿到内容（含命中缓存）才扣；整条链失败分文不取。
//
// in.Force 跳过缓存读、直接重取一次（用户侧的「强制刷新」），不改变价格。
//
// 上游确认作品本身不可见（errHidden）时记下来（parse_hidden），之后同一作品的普通请求直接回
// ErrItemHidden、不调上游：失败不收用户的钱，上游却每次照收。强制刷新不受它挡。
//
// 提交的是口令（in.ShareCode）时，先认平台、过余额预检，再花一次上游调用把口令换成作品链接，
// 之后与普通链接一样；那次调用记在这笔解析记录的最前面（resolveShareCode）。
func (s *Service) Parse(ctx context.Context, userID uuid.UUID, in ParseInput) (*ParseResponse, error) {
	start := time.Now()
	t := Target{URI: in.URI, Resolved: in.URI}

	var p Parser
	if in.ShareCode != "" {
		var code string
		var ok bool
		if p, code, ok = s.registry.FindShareCode(in.ShareCode); !ok {
			return nil, ErrNoParser
		}
		t = Target{URI: in.ShareCode, Code: code}
	} else {
		// 分享短链先跟跳转拿到作品链接（免费，不碰上游），缓存键才能是作品 ID
		if sp, ok := s.registry.FindShortLink(in.URI); ok {
			resolved, err := s.resolver.Resolve(ctx, in.URI, func(u string) bool {
				_, ok := sp.Match(u)
				return ok
			})
			if err != nil {
				s.writeLog(ctx, &LogEntry{Provider: sp.Provider(), ResourceID: truncateRunes(in.URI, 200),
					Status: LogFailed, Error: err.Error()}, start)
				return nil, ErrParseFailed
			}
			t.Resolved = resolved
		}
		var ok bool
		if p, t.ResourceID, ok = s.registry.Find(t.Resolved); !ok {
			return nil, ErrNoParser // 无法解析的 URI 不产生费用
		}
	}

	eps, err := s.endpointsFor(ctx, p.Provider())
	if err != nil {
		return nil, err
	}
	price, err := s.price(ctx, p.Provider(), eps)
	if err != nil {
		return nil, err
	}
	if pf, ok := p.(PriceFactorer); ok {
		price *= pf.PriceFactor(t)
	}
	if price > 0 {
		bal, err := s.wallet.Balance(ctx, userID)
		if err != nil {
			return nil, err
		}
		if bal < price {
			return nil, &wallet.InsufficientCreditsError{Balance: bal, Required: price}
		}
	}

	var pre []Attempt // 换口令的调用：它属于这个请求自己，缓存命中、搭车合并都照记
	if t.Code != "" {
		resolved, attempts, err := s.resolveShareCode(ctx, p, eps, t)
		if errors.Is(err, errNoResolver) {
			return nil, ErrNoParser
		}
		if err != nil {
			s.writeLog(ctx, &LogEntry{Provider: p.Provider(), ResourceID: "口令:" + t.Code, Status: LogFailed,
				Error: err.Error(), Attempts: attempts, CostMicros: attemptsCost(attempts)}, start)
			return nil, ErrParseFailed
		}
		t.Resolved = resolved
		t.ResourceID, _ = p.Match(resolved)
		pre = attempts
		eps = deductSpent(eps, attempts)
	}
	resourceID := t.ResourceID

	entry := &LogEntry{Provider: p.Provider(), ResourceID: resourceID, Forced: in.Force,
		Attempts: pre, CostMicros: attemptsCost(pre)}
	resp := &ParseResponse{Provider: p.Provider(), ResourceID: resourceID}

	// 强制刷新在冷却期内被降级为一次普通读，而不是被拒绝：见 Service.refresh 的说明。
	// 不可见记录同样只挡普通读：强制刷新是用户主动要求重取，照常放行。
	bypassCache := in.Force && s.refresh.Allow(p.Provider()+"\x00"+resourceID)
	if !bypassCache {
		// 先看不可见记录再看缓存：两者并存只可能是记录写在缓存之后（重取到内容会删掉记录），以新的为准。
		if reason, hidden, err := s.cache.Hidden(ctx, p.Provider(), resourceID); err != nil {
			slog.Warn("parse_hidden 读取失败", "err", err)
		} else if hidden {
			entry.Status = LogFailed
			entry.Error = "已确认作品本身不可见，未调上游：" + reason
			s.writeLog(ctx, entry, start)
			return nil, ErrItemHidden
		}
		if cached, version, hit, err := s.cache.Get(ctx, p.Provider(), resourceID); err != nil {
			slog.Warn("parse_cache 读取失败", "err", err)
		} else if hit {
			entry.Cached = true
			resp.Snippet, resp.Version, resp.Cached = cached, version, true
			return s.settle(ctx, userID, price, start, entry, resp)
		}
	}

	out := s.fetchShared(ctx, p, eps, t)
	entry.Source = out.source
	entry.Shared = !out.leader
	if out.leader {
		// 并发合并时只有真正发起调用的那个请求承担成本，否则同一笔钱会被记 N 次
		entry.Attempts = joinAttempts(pre, out.attempts)
		entry.CostMicros += attemptsCost(out.attempts)
	}
	if out.err != nil {
		entry.Status = LogFailed
		entry.Error = out.err.Error()
		s.writeLog(ctx, entry, start)
		if errors.Is(out.err, errHidden) {
			return nil, ErrItemHidden
		}
		return nil, ErrParseFailed
	}
	resp.Snippet, resp.Version = out.snip, out.version
	return s.settle(ctx, userID, price, start, entry, resp)
}

// settle 扣费并记一笔解析记录。
//
// 开头已经确认过余额，这里仍可能扣不动：同一用户的并发请求在这期间把钱花掉了。
// 内容不给（不付钱就没有内容），但它已经进了缓存，成本没有白花；记录标为 unpaid，
// 对账时看得见这笔「有成本、没收入」。
func (s *Service) settle(ctx context.Context, userID uuid.UUID, price int64, start time.Time,
	entry *LogEntry, resp *ParseResponse) (*ParseResponse, error) {
	if price > 0 {
		if _, err := s.wallet.Consume(ctx, userID, price, "parse_uri:"+entry.Provider); err != nil {
			// 原样往上抛，**不要**替换成裸的 wallet.ErrInsufficientCredits：
			// Consume 返回的 *InsufficientCreditsError 带着 Balance/Required，
			// handler 靠 errors.As 拿它告诉客户端「还差多少」。
			entry.Status = LogUnpaid
			entry.Error = err.Error()
			if !errors.Is(err, wallet.ErrInsufficientCredits) {
				slog.Warn("parse credits 扣费失败", "err", err)
			}
			s.writeLog(ctx, entry, start)
			return nil, err
		}
	}
	entry.Status = LogOK
	entry.CreditsCharged = price
	resp.CreditsCharged = price
	s.writeLog(ctx, entry, start)
	return resp, nil
}

func (s *Service) writeLog(ctx context.Context, e *LogEntry, start time.Time) {
	if s.logs == nil {
		return
	}
	e.LatencyMs = int(time.Since(start).Milliseconds())
	// 客户端断开也要记下来：成本可能已经发生
	if err := s.logs.Write(context.WithoutCancel(ctx), e); err != nil {
		slog.Warn("解析记录写入失败", "provider", e.Provider, "err", err)
	}
}

// fetchOutcome 是一次兜底链执行的结果。leader 只对真正执行了链的那个请求为 true。
type fetchOutcome struct {
	snip     *ParsedSnippet
	version  string
	source   string
	attempts []Attempt
	err      error
	leader   bool
}

// fetchShared 跑兜底链并写缓存，同一资源的并发调用合并为一次。
//
// 整个「抓取 → 组装 → 写缓存」都放进 singleflight 内部，而不是只包住抓取：
// 否则 N 个等待者会各自再写一次缓存，把省下来的上游调用换成 N 次重复写。
//
// 内部用 context.WithoutCancel：首个调用者断开连接不应让其余等待者一起失败；
// 上游的钱一旦花出去，结果也该留进缓存。时长由各来源自己的超时兜住
// （接口的 timeout_ms、内置实现的 http.Client Timeout），不会留下永不返回的调用。
func (s *Service) fetchShared(ctx context.Context, p Parser, eps []extapi.Endpoint, t Target) fetchOutcome {
	// singleflight 只在发起者自己的 goroutine 里执行闭包，等待者的闭包从不运行，
	// 所以这个局部变量只会被发起者置真，无需同步。
	leader := false
	v, _, _ := s.flight.Do(p.Provider()+"\x00"+t.ResourceID, func() (any, error) {
		leader = true
		fctx := context.WithoutCancel(ctx)

		res, source, attempts, err := s.runChain(fctx, p, eps, t)
		if err != nil {
			// 作品本身不可见是一个付过钱的结论：记下来，之后的普通请求不必再为它付一次
			if errors.Is(err, errHidden) {
				if err := s.cache.PutHidden(fctx, p.Provider(), t.ResourceID, err.Error()); err != nil {
					slog.Warn("parse_hidden 写入失败", "err", err)
				}
			}
			return fetchOutcome{attempts: attempts, err: err}, nil
		}

		snippetType := p.SnippetType()
		subtype := p.Provider()
		if snippetType == "url" {
			// url 类型不需要 subtype，避免与 special 类型混淆
			subtype = ""
		}
		snip := &ParsedSnippet{
			Type:        snippetType,
			Subtype:     subtype,
			Title:       res.Title,
			Description: res.Description,
			Payload:     res.Payload,
			SourceType:  "upstream",
			SourceData:  res.SourceData,
			Assets:      res.Assets,
		}
		if err := s.cache.Put(fctx, p.Provider(), t.ResourceID, res.Version, snip, res.CacheTTL); err != nil {
			// 缓存失败不影响返回结果
			slog.Warn("parse_cache 写入失败", "err", err)
		}
		// 取到了内容（强制刷新时可能是之前被确认不可见的作品）：作品又能看了，记录作废
		if err := s.cache.ClearHidden(fctx, p.Provider(), t.ResourceID); err != nil {
			slog.Warn("parse_hidden 删除失败", "err", err)
		}
		return fetchOutcome{snip: snip, version: res.Version, source: source, attempts: attempts}, nil
	})
	out := v.(fetchOutcome)
	out.leader = leader
	return out
}

var (
	errNoSource       = errors.New("没有可用来源")
	errChainExhausted = errors.New("所有来源均失败")
)

// runChain 按价格从低到高尝试各来源，直到拿到可用内容：内置免费来源在前，
// 付费接口按单价升序（同价看 priority，见 extapi.Repo.ListForProvider）。
// 某个接口确认作品本身不可见（errHidden）时提前结束：换接口也一样，后面的记成跳过。
//
// 每一步都记成一次 Attempt，包括被跳过的 —— 「为什么没走到那个接口」在排查时
// 和「那个接口为什么失败」一样重要。
func (s *Service) runChain(ctx context.Context, p Parser, eps []extapi.Endpoint, t Target) (*ParseResult, string, []Attempt, error) {
	var attempts []Attempt
	add := func(a Attempt) {
		a.Seq = len(attempts) + 1
		attempts = append(attempts, a)
	}

	if b, ok := p.(BuiltinParser); ok {
		source := "builtin:" + p.Provider()
		t0 := time.Now()
		res, err := b.Parse(ctx, t.Resolved, t.ResourceID)
		a := Attempt{Source: source, LatencyMs: int(time.Since(t0).Milliseconds())}
		switch {
		case err != nil:
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindBuiltin, err.Error()
		case !usable(res):
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindEmpty, "没有标题也没有正文"
		default:
			a.Outcome = AttemptOK
			add(a)
			return res, source, attempts, nil
		}
		add(a)
	}

	// 同一账户下的接口共用余额，而 eps 里的余额是链开始前读的：链内前面的调用
	// 花掉的钱要自己扣回去，否则「余额耗尽就跳过」和余额快照都会慢一拍。
	spent := map[uuid.UUID]int64{}
	var hidden error // 某个接口已确认作品本身不可见（errHidden）：后面的都跳过
	for _, ep := range eps {
		if s.adapters.isResolver(ep) {
			continue // 换口令的接口不取内容，口令那一步已经用过它（resolveShareCode）
		}
		ep.BalanceMicros -= spent[ep.AccountID]
		epID, accID := ep.ID, ep.AccountID
		a := Attempt{Source: ep.Slug, EndpointID: &epID, AccountID: &accID, AccountName: ep.AccountName}

		if hidden != nil {
			a.Outcome, a.ErrorKind, a.Error = AttemptSkipped, extapi.KindUnavailable, "前面的接口已确认作品本身不可见"
			add(a)
			continue
		}
		ad, ok := s.adapters.callable(ep)
		if !ok {
			a.Outcome, a.ErrorKind = AttemptSkipped, extapi.KindNoAdapter
			a.Error = fmt.Sprintf("代码里没有 %s/%s 的适配器 %q", ep.Vendor, ep.Provider, ep.Slug)
			add(a)
			continue
		}
		if ad.Accept != nil && !ad.Accept(t) {
			a.Outcome, a.ErrorKind, a.Error = AttemptSkipped, extapi.KindNotApplicable, "这条链接不适用此接口"
			add(a)
			continue
		}
		if ep.UnitPriceMicros > 0 && ep.BalanceMicros <= 0 {
			a.Outcome, a.ErrorKind, a.Error = AttemptSkipped, extapi.KindNoBalance, "账户余额已用完"
			add(a)
			continue
		}

		call := s.extapi.Call(ctx, ep, ad.Request(t), t.ResourceID)
		spent[ep.AccountID] += call.CostMicros
		a.HTTPStatus, a.LatencyMs, a.CostMicros, a.SampleID = call.HTTPStatus, call.LatencyMs, call.CostMicros, call.SampleID
		if call.CostMicros > 0 {
			bal := call.BalanceAfter
			a.BalanceAfter = &bal
		}
		if call.Err != nil {
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, call.Err.Kind, call.Err.Msg
			add(a)
			continue
		}
		res, err := ad.Decode(call.Data, t)
		switch {
		case errors.Is(err, errUnavailable):
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindUnavailable, err.Error()
			if errors.Is(err, errHidden) {
				hidden = err
			}
		case err != nil:
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindDecode, err.Error()
		case !usable(res):
			a.Outcome, a.ErrorKind, a.Error = AttemptFailed, extapi.KindDecode, "转换后没有标题也没有正文"
		default:
			a.Outcome = AttemptOK
			add(a)
			return res, ep.Slug, attempts, nil
		}
		add(a)
	}

	if len(attempts) == 0 {
		return nil, "", nil, errNoSource
	}
	if hidden != nil {
		return nil, "", attempts, hidden // 记录的总原因直接写明作品不可见，而不是「所有来源均失败」
	}
	return nil, "", attempts, errChainExhausted
}

// usable 判断一份结果能不能交给用户：至少要有标题或正文。
// 上游降级（限流、部分字段缺失）时常见「200 但几乎是空的」，那不算成功。
func usable(res *ParseResult) bool {
	if res == nil {
		return false
	}
	return (res.Title != nil && *res.Title != "") || (res.Description != nil && *res.Description != "")
}

// endpointsFor 取某平台的付费兜底链；没接 extapi 时为空。
func (s *Service) endpointsFor(ctx context.Context, provider string) ([]extapi.Endpoint, error) {
	if s.extapi == nil {
		return nil, nil
	}
	return s.extapi.ListForProvider(ctx, provider)
}

// price 给出某平台此刻的售价，规则见 price.go。
func (s *Service) price(ctx context.Context, provider string, eps []extapi.Endpoint) (int64, error) {
	if s.defaultPrice <= 0 {
		return 0, nil // 计费整体关闭
	}
	if s.prices != nil {
		credits, ok, err := s.prices.Get(ctx, provider)
		if err != nil {
			return 0, err
		}
		if ok {
			return credits, nil
		}
	}
	return autoPrice(s.adapters.maxUnitPrice(eps), s.defaultPrice)
}

// Manifest 导出此刻真有来源的平台的匹配规则，供 /uris/manifest 下发。
// 口令规则另要平台有能用的换口令接口：没有它，客户端交上来的口令注定解析不了。
func (s *Service) Manifest(ctx context.Context) (Manifest, error) {
	paid, resolvers := map[string]bool{}, map[string]bool{}
	if s.extapi != nil {
		eps, err := s.extapi.ListEnabled(ctx)
		if err != nil {
			return Manifest{}, err
		}
		for _, ep := range eps {
			if a, ok := s.adapters.callable(ep); ok {
				if a.Resolve != nil {
					resolvers[ep.Provider] = true
				} else {
					paid[ep.Provider] = true
				}
			}
		}
	}
	return s.registry.Manifest(func(p Parser) bool {
		_, builtin := p.(BuiltinParser)
		return builtin || paid[p.Provider()]
	}, func(p Parser) bool { return resolvers[p.Provider()] }), nil
}

// ---- 管理后台 ----

// Prices 列出每个平台的售价构成，按注册顺序。
func (s *Service) Prices(ctx context.Context) ([]PriceInfo, error) {
	var eps []extapi.Endpoint
	if s.extapi != nil {
		var err error
		if eps, err = s.extapi.ListEnabled(ctx); err != nil {
			return nil, err
		}
	}
	overrides := map[string]int64{}
	if s.prices != nil {
		var err error
		if overrides, err = s.prices.All(ctx); err != nil {
			return nil, err
		}
	}

	byProvider := map[string][]extapi.Endpoint{}
	for _, ep := range eps {
		byProvider[ep.Provider] = append(byProvider[ep.Provider], ep)
	}

	out := make([]PriceInfo, 0, len(s.registry.Parsers()))
	for _, p := range s.registry.Parsers() {
		list := byProvider[p.Provider()]
		info := PriceInfo{Provider: p.Provider(), MaxUnitPriceMicros: s.adapters.maxUnitPrice(list)}
		_, info.HasBuiltin = p.(BuiltinParser)
		for _, ep := range list {
			if _, ok := s.adapters.source(ep); ok {
				info.Endpoints++
			}
		}
		auto, err := autoPrice(info.MaxUnitPriceMicros, s.defaultPrice)
		if err != nil {
			return nil, err
		}
		info.AutoCredits, info.EffectiveCredits = auto, auto
		if v, ok := overrides[p.Provider()]; ok {
			info.OverrideCredits = &v
			info.EffectiveCredits = v
		}
		if s.defaultPrice <= 0 {
			info.EffectiveCredits = 0
		}
		if pf, ok := p.(PriceFactorer); ok {
			info.Note = pf.PriceNote()
		}
		info.Loss = info.EffectiveCredits*pricing.MicrosPerCredit < info.MaxUnitPriceMicros
		out = append(out, info)
	}
	return out, nil
}

// SetPrice 设定某平台的售价覆盖值。
func (s *Service) SetPrice(ctx context.Context, provider string, credits int64) error {
	if !s.hasProvider(provider) {
		return ErrNoParser
	}
	if credits <= 0 {
		return ErrBadPrice
	}
	return s.prices.Set(ctx, provider, credits)
}

// ClearPrice 撤销覆盖值，回到自动价。
func (s *Service) ClearPrice(ctx context.Context, provider string) error {
	return s.prices.Delete(ctx, provider)
}

func (s *Service) hasProvider(provider string) bool {
	for _, p := range s.registry.Parsers() {
		if p.Provider() == provider {
			return true
		}
	}
	return false
}

// ErrBadPrice 表示售价不是正整数。
var ErrBadPrice = errors.New("price must be a positive number of credits")

// Logs 列出解析记录。某一步的采样和示例比出缺失或新增时，在那一步带上差异计数与后台是否
// 看过，后台列表据此标圆点。命中缓存的记录不带：那份内容的差异标在当初取到它的那条上。
// 比对失败不影响列表，只是不标。
func (s *Service) Logs(ctx context.Context, f LogFilter) ([]LogEntry, error) {
	logs, err := s.logs.List(ctx, f)
	if err != nil || s.extapi == nil {
		return logs, err
	}
	var ids []uuid.UUID
	for _, l := range logs {
		for _, a := range l.Attempts {
			if a.SampleID != nil {
				ids = append(ids, *a.SampleID)
			}
		}
	}
	drifts, err := s.extapi.SampleDrifts(ctx, ids)
	if err != nil {
		slog.Warn("解析记录的采样结构比对失败", "err", err)
		return logs, nil
	}
	for i := range logs {
		for j := range logs[i].Attempts {
			a := &logs[i].Attempts[j]
			if a.SampleID == nil {
				continue
			}
			if d, ok := drifts[*a.SampleID]; ok {
				a.Drift = &d
			}
		}
	}
	return logs, nil
}

func (s *Service) LogSummary(ctx context.Context, days int) ([]ProviderSummary, []SourceSummary, error) {
	return s.logs.Summary(ctx, days)
}
