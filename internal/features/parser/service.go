package parser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/wallet"
)

type Service struct {
	registry  *Registry
	cache     *Cache
	wallet    *wallet.Service
	extapi    *extapi.Service // 第三方付费 API 供应层；nil 时全部走内置免费实现
	parseCost int64           // 每次解析请求扣减的 credits（命中/未命中一致）；<=0 时不扣
}

func NewService(registry *Registry, cache *Cache, walletSvc *wallet.Service, extapiSvc *extapi.Service, parseCost int64) *Service {
	return &Service{registry: registry, cache: cache, wallet: walletSvc, extapi: extapiSvc, parseCost: parseCost}
}

// Manifest 透传 registry 的受支持解析清单，供 handler 的公开 /uris/manifest 端点导出。
func (s *Service) Manifest() Manifest { return s.registry.Manifest() }

// Parse 是 Service 主入口：先按 URI 选 provider → 扣费 → 命中缓存即返回 → 否则抓取 → 写缓存
//
// 计费策略（2026-07-05 起）：**每次解析请求都按 parseCost 扣 credits，命中/未命中一致**。
// 故扣费点前移到缓存查询之前——改为「按请求计价」，用户侧价格模型更简单、更可预期。
// （原「命中免费」设计已废止，见 ADR-046 更新。）parseCost<=0 时整体不扣。
// 余额不足返 wallet.ErrInsufficientCredits（handler 映射到 402）。
func (s *Service) Parse(ctx context.Context, userID uuid.UUID, uri string) (*ParseResponse, error) {
	p, resourceID, ok := s.registry.Find(uri)
	if !ok {
		return nil, ErrNoParser // 无法解析的 URI 不产生费用
	}

	// 先扣费再查缓存：命中也计费。放在 registry.Find 之后，保证只有「可解析」的请求才扣。
	if s.parseCost > 0 {
		reason := "parse_uri:" + p.Provider()
		if _, err := s.wallet.Consume(ctx, userID, s.parseCost, reason); err != nil {
			if errors.Is(err, wallet.ErrInsufficientCredits) {
				return nil, wallet.ErrInsufficientCredits
			}
			slog.Warn("parse credits 扣费失败", "err", err)
			return nil, err
		}
	}

	if cached, hit, err := s.cache.Get(ctx, p.Provider(), resourceID); err != nil {
		slog.Warn("parse_cache 读取失败", "err", err)
	} else if hit {
		return &ParseResponse{
			Provider:   p.Provider(),
			ResourceID: resourceID,
			Snippet:    cached,
			Cached:     true,
		}, nil
	}

	res, err := s.fetch(ctx, p, uri, resourceID)
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

	if err := s.cache.Put(ctx, p.Provider(), resourceID, snip); err != nil {
		// 缓存失败不影响返回结果
		slog.Warn("parse_cache 写入失败", "err", err)
	}

	return &ParseResponse{
		Provider:   p.Provider(),
		ResourceID: resourceID,
		Snippet:    snip,
		Cached:     false,
	}, nil
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
