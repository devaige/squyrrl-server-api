package parser

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/wallet"
)

type Service struct {
	registry  *Registry
	cache     *Cache
	wallet    *wallet.Service
	cacheCost int64 // 每次 cache-miss 真上行解析时扣减的 credits；<=0 时不扣
}

func NewService(registry *Registry, cache *Cache, walletSvc *wallet.Service, cacheCost int64) *Service {
	return &Service{registry: registry, cache: cache, wallet: walletSvc, cacheCost: cacheCost}
}

// Parse 是 Service 主入口：先按 URI 选 provider → 命中缓存即返回 → 否则扣费 → 抓取 → 写缓存
//
// cache 命中（cached=true）**不扣 credits**：跨用户复用已付过钱的解析结果。
// cache miss → 触发上游真实调用 → 扣 cacheCost 个 credits（per ADR-013）。
// 余额不足返 wallet.ErrInsufficientCredits（handler 映射到 402）。
func (s *Service) Parse(ctx context.Context, userID uuid.UUID, uri string) (*ParseResponse, error) {
	p, resourceID, ok := s.registry.Find(uri)
	if !ok {
		return nil, ErrNoParser
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

	// cache miss → 扣费（dev 配置 cacheCost=0 时跳过）
	if s.cacheCost > 0 {
		reason := "parse_uri:" + p.Provider()
		if _, err := s.wallet.Consume(ctx, userID, s.cacheCost, reason); err != nil {
			if errors.Is(err, wallet.ErrInsufficientCredits) {
				return nil, wallet.ErrInsufficientCredits
			}
			slog.Warn("parse credits 扣费失败", "err", err)
			return nil, err
		}
	}

	res, err := p.Parse(ctx, uri, resourceID)
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
