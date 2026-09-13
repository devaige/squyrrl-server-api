package parser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/ratelimit"
)

type Service struct {
	registry *Registry
	cache    *Cache
	wallet   *wallet.Service
	extapi   *extapi.Service // 第三方付费 API 供应层；nil 时全部走内置免费实现

	// baseCost 是**内置免费 provider**（YouTube oEmbed / Gist / Reddit / GenericOG）
	// 的解析单价。付费 endpoint 的价格不走这里，由 extapi.Quote 从上游单价 × 加价
	// 倍率算出（pricing.CreditCost）。<=0 表示整体关闭解析计费（dev 用）。
	baseCost int64

	// flight 让并发解析**同一资源**的请求共用一次上游调用。
	//
	// 这不是锦上添花的优化：付费 endpoint 的余额是 soft budget，没有 advisory lock，
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

func NewService(
	registry *Registry, cache *Cache, walletSvc *wallet.Service,
	extapiSvc *extapi.Service, baseCost int64, refreshCooldown time.Duration,
) *Service {
	// 每个资源每个冷却窗口放行一次真正的重取。冷却期为 0 时走 ratelimit 自己的
	// 「limit<=0 即 Allow 恒真」而不是依赖 period=0 的时间比较 ——
	// 后者要靠两次调用落在不同纳秒上才成立，是个能跑通但说不清楚的关闭方式。
	limit := 1
	if refreshCooldown <= 0 {
		limit = 0
	}
	return &Service{
		registry: registry, cache: cache, wallet: walletSvc,
		extapi: extapiSvc, baseCost: baseCost,
		refresh: ratelimit.New(limit, refreshCooldown),
	}
}

// Manifest 透传 registry 的受支持解析清单，供 handler 的公开 /uris/manifest 端点导出。
func (s *Service) Manifest() Manifest { return s.registry.Manifest() }

// Parse 是 Service 主入口：先按 URI 选 provider → 询价 → 扣费 → 命中缓存即返回 → 否则抓取 → 写缓存
//
// in.Force 跳过缓存读、直接重取一次（用户侧的「强制刷新」）。它**不改变计价** ——
// 扣费点在缓存查询之前，强制与否付一样的钱，区别只是拿到的是不是新鲜数据。
//
// 计费策略（2026-07-05 起）：**每次解析请求都扣 credits，命中/未命中一致**。
// 故扣费点前移到缓存查询之前——改为「按请求计价」，用户侧价格模型更简单、更可预期。
// （原「命中免费」设计已废止，见 ADR-046 更新。）
//
// 价格从 2026-09-12 起由 quote 算出，不再是一个全局常量：走付费 endpoint 的
// provider 按「上游单价 × 加价倍率」收（pricing.CreditCost），走内置免费实现的
// 按 baseCost 收。baseCost<=0 时整体不扣。
// 余额不足返 wallet.ErrInsufficientCredits（handler 映射到 402）。
func (s *Service) Parse(ctx context.Context, userID uuid.UUID, in ParseInput) (*ParseResponse, error) {
	p, resourceID, ok := s.registry.Find(in.URI)
	if !ok {
		return nil, ErrNoParser // 无法解析的 URI 不产生费用
	}

	// 先扣费再查缓存：命中也计费。放在 registry.Find 之后，保证只有「可解析」的请求才扣。
	cost := s.quote(ctx, p.Provider())
	if cost > 0 {
		reason := "parse_uri:" + p.Provider()
		if _, err := s.wallet.Consume(ctx, userID, cost, reason); err != nil {
			// 原样往上抛，**不要**替换成裸的 wallet.ErrInsufficientCredits。
			// Consume 返回的是带 Balance/Required 的 *InsufficientCreditsError，
			// 换成哨兵会让 handler 的 errors.As 永远落空，402 里的「还差多少」恒为 0。
			// 价格还是常量时这只是不好看，现在价格随 provider 变，客户端再也猜不出来。
			// errors.Is 不受影响 —— 那个类型 Unwrap 到同一个哨兵。
			if !errors.Is(err, wallet.ErrInsufficientCredits) {
				slog.Warn("parse credits 扣费失败", "err", err)
			}
			return nil, err
		}
	}

	// 强制刷新在冷却期内被降级为一次普通读，而不是被拒绝：见 Service.refresh 的说明。
	bypassCache := in.Force && s.refresh.Allow(p.Provider()+"\x00"+resourceID)

	if !bypassCache {
		if cached, version, hit, err := s.cache.Get(ctx, p.Provider(), resourceID); err != nil {
			slog.Warn("parse_cache 读取失败", "err", err)
		} else if hit {
			return &ParseResponse{
				Provider:       p.Provider(),
				ResourceID:     resourceID,
				Snippet:        cached,
				Version:        version,
				Cached:         true,
				CreditsCharged: cost,
			}, nil
		}
	}

	snip, version, err := s.fetchShared(ctx, p, in.URI, resourceID)
	if err != nil {
		return nil, err
	}

	return &ParseResponse{
		Provider:       p.Provider(),
		ResourceID:     resourceID,
		Snippet:        snip,
		Version:        version,
		Cached:         false,
		CreditsCharged: cost,
	}, nil
}

// fetchShared 抓取并写缓存，同一资源的并发调用合并为一次。
//
// 整个「抓取 → 组装 → 写缓存」都放进 singleflight 内部，而不是只包住抓取：
// 否则 N 个等待者会各自再写一次缓存，把省下来的上游调用换成 N 次重复写。
//
// 内部用 context.WithoutCancel：首个调用者断开连接不应让其余等待者一起失败。
// 更重要的是**钱已经扣了** —— 扣费发生在这之前，此刻放弃抓取等于用户付了钱却
// 连缓存都没留下。上游侧有各自的超时（extapi 的 TimeoutMs、内置实现的 http.Client
// Timeout）兜住时长，所以去掉取消不会留下一个永不返回的调用。
func (s *Service) fetchShared(ctx context.Context, p Parser, uri, resourceID string) (*ParsedSnippet, string, error) {
	type outcome struct {
		snip    *ParsedSnippet
		version string
	}

	v, err, _ := s.flight.Do(p.Provider()+"\x00"+resourceID, func() (any, error) {
		fctx := context.WithoutCancel(ctx)

		res, err := s.fetch(fctx, p, uri, resourceID)
		if err != nil {
			return nil, err
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

		if err := s.cache.Put(fctx, p.Provider(), resourceID, res.Version, snip); err != nil {
			// 缓存失败不影响返回结果
			slog.Warn("parse_cache 写入失败", "err", err)
		}
		return outcome{snip: snip, version: res.Version}, nil
	})
	if err != nil {
		return nil, "", err
	}
	out := v.(outcome)
	return out.snip, out.version, nil
}

// quote 算出这次解析要扣多少 credits。
//
// 三条路径都落在「与 fetch 实际会走哪条一致」这一个要求上：
//   - 有可用的付费 endpoint → 按 extapi 的报价收（已含上游成本 × 加价倍率）；
//   - ErrNoEndpoint → fetch 也会回退到内置免费实现，按 baseCost 收；
//   - 询价出错（多半是数据库） → fetch 里的 ListForProvider 同样会失败，
//     它会 fall through 到内置实现，所以 baseCost 仍然是对的那一边。
//
// 换句话说：询价失败不阻断解析，也不会静默免单。
func (s *Service) quote(ctx context.Context, provider string) int64 {
	if s.baseCost <= 0 {
		return 0 // 计费整体关闭
	}
	if s.extapi == nil {
		return s.baseCost
	}
	cost, err := s.extapi.Quote(ctx, provider)
	switch {
	case err == nil:
		return cost
	case errors.Is(err, extapi.ErrNoEndpoint):
		// 正常路径：这个 provider 还没接付费供应商。
	default:
		slog.Warn("extapi 询价失败，按内置实现计价", "provider", provider, "err", err)
	}
	return s.baseCost
}

// fetch 先走已配置的付费 endpoint 兜底链；若该 provider 没有可用 endpoint（ErrNoEndpoint）
// 或整条链都失败，则回退到 Parser 的内置免费实现（如 YouTube oEmbed）。
// 这样「有配置就用付费拿丰富数据、没配置/挂了仍能返回基础数据」自然成立，且对既有 provider 零影响。
func (s *Service) fetch(ctx context.Context, p Parser, uri, resourceID string) (*ParseResult, error) {
	if s.extapi != nil {
		fr, err := s.extapi.Fetch(ctx, p.Provider(), uri, resourceID)
		if err == nil {
			return fetchToResult(p.Provider(), uri, resourceID, fr), nil
		}
		if !errors.Is(err, extapi.ErrNoEndpoint) {
			// 全链失败（网络/额度/映射）不直接报错：降级到内置实现，保证可用性
			slog.Warn("extapi 兜底链失败，回退内置解析", "provider", p.Provider(), "err", err)
		}
	}
	return p.Parse(ctx, uri, resourceID)
}

// fetchToResult 把供应层的中立结果组装成 provider-specific 的 ParseResult。
// payload 用与内置实现兼容的通用键（url/resource_id/thumbnail_url），
// source_data 记作者信息并附上实际命中的 endpoint（via）便于回溯。
func fetchToResult(provider, uri, resourceID string, fr *extapi.FetchResult) *ParseResult {
	payload, _ := json.Marshal(map[string]any{
		"url":           uri,
		"resource_id":   resourceID,
		"thumbnail_url": fr.ThumbnailURL,
	})
	source, _ := json.Marshal(map[string]any{
		"provider":    provider,
		"author_name": fr.AuthorName,
		"author_url":  fr.AuthorURL,
		"via":         fr.EndpointSlug,
	})
	res := &ParseResult{
		Payload:    payload,
		SourceData: source,
		Assets:     mediaAsset(fr.ThumbnailURL, "thumbnail", ""),
		Version:    fr.Version,
	}
	if fr.Title != "" {
		t := fr.Title
		res.Title = &t
	}
	if fr.Description != "" {
		d := fr.Description
		res.Description = &d
	}
	return res
}
