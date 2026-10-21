-- +goose Up
-- =============================================================================
-- 抖音分享口令（2026-10-21）：抖音文章的分享面板只有「复制口令」，复制出来的文字里没有链接。
-- 这个接口把口令换成文章 ID（即作品 ID），之后照常走抖音的单作品接口取正文。
--
-- 对应 parser/tikhub.go 里的换口令适配器（Adapter.Resolve）：它不进取内容的兜底链、不抬售价，
-- 只决定解析清单里下不下发抖音的口令规则。口令解析按 2 倍收（platforms.go，priceFactor）。
-- 单价暂按抖音其他接口的 $0.001 记（未取 TikHub 实时报价），以后台核对为准。
-- =============================================================================

INSERT INTO api_endpoints (account_id, provider, slug, priority, enabled, unit_price_micros, config)
SELECT a.id, 'douyin', 'tikhub_douyin_app_v3_fetch_share_info_by_share_code', 100, TRUE, 1000, '{"archive_live": true}'::jsonb
FROM api_accounts a
WHERE a.slug = 'tikhub'
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
DELETE FROM api_endpoints WHERE slug = 'tikhub_douyin_app_v3_fetch_share_info_by_share_code';
