-- +goose Up
-- =============================================================================
-- 第一批 TikHub 接口（ADR-080，2026-10-16 用户选定）：只接「取单条作品」的接口。
--
-- 每行对应 parser/tikhub.go 里的一个适配器（slug 相同），单价取自 TikHub
-- get_all_endpoints_info 的实时报价（美元 micros）。兜底链按单价升序，同价看 priority。
--
-- enabled 按推荐：
--   - 抖音网页 V2 不启用（与网页 V1 重复）；
--   - Instagram 只启用 v1（$0.001）。v2 $0.002 / v3 $0.008 会把该平台的自动售价
--     从 20 抬到 40 / 160 代币；实测 v1 对部分帖子会误报「不存在」，需要更高成功率时
--     在后台启用 v2；
--   - 快手只启用 App 版（$0.001），网页版三个 $0.002 先不启用。
-- 售价：没在后台设定时 = 该平台已启用接口的最高单价 × 2，例如小红书、公众号 200 代币，其余 20。
--
-- archive_live：接入初期把线上调用采样进 api_samples（每接口最多 20 条），
-- 用来核对上游有没有悄悄改字段；稳定后可在后台关掉。
-- =============================================================================

INSERT INTO api_endpoints (account_id, provider, slug, priority, enabled, unit_price_micros, config)
SELECT a.id, v.provider, v.slug, v.priority, v.enabled, v.price, '{"archive_live": true}'::jsonb
FROM api_accounts a
CROSS JOIN (VALUES
    ('twitter',     'tikhub_twitter_web_fetch_tweet_detail',            100, TRUE,   1000),

    ('tiktok',      'tikhub_tiktok_app_v3_fetch_one_video',             100, TRUE,   1000),
    ('tiktok',      'tikhub_tiktok_app_v3_fetch_one_video_v2',          110, TRUE,   1000),
    ('tiktok',      'tikhub_tiktok_app_v3_fetch_one_video_v3',          120, TRUE,   1000),
    ('tiktok',      'tikhub_tiktok_web_fetch_post_detail',              130, TRUE,   1000),
    ('tiktok',      'tikhub_tiktok_web_fetch_post_detail_v2',           140, TRUE,   1000),

    ('douyin',      'tikhub_douyin_app_v3_fetch_one_video',             100, TRUE,   1000),
    ('douyin',      'tikhub_douyin_app_v3_fetch_one_video_v2',          110, TRUE,   1000),
    ('douyin',      'tikhub_douyin_app_v3_fetch_one_video_v3',          120, TRUE,   1000),
    ('douyin',      'tikhub_douyin_web_fetch_one_video',                130, TRUE,   1000),
    ('douyin',      'tikhub_douyin_web_fetch_one_video_v2',             140, FALSE,  1000),

    -- 视频接口在前：它只接分享链接标明 type=video 的，其余落到图文接口
    ('xiaohongshu', 'tikhub_xiaohongshu_app_v2_get_video_note_detail',  100, TRUE,  10000),
    ('xiaohongshu', 'tikhub_xiaohongshu_app_v2_get_image_note_detail',  110, TRUE,  10000),

    ('bilibili',    'tikhub_bilibili_web_fetch_one_video',              100, TRUE,   1000),
    ('bilibili',    'tikhub_bilibili_app_fetch_one_video',              110, TRUE,   1000),

    ('weibo',       'tikhub_weibo_web_v2_fetch_post_detail',            100, TRUE,   1000),
    ('weibo',       'tikhub_weibo_app_fetch_status_detail',             110, TRUE,   1000),

    ('instagram',   'tikhub_instagram_v1_fetch_post_by_url',            100, TRUE,   1000),
    ('instagram',   'tikhub_instagram_v1_fetch_post_by_id',             110, TRUE,   1000),
    ('instagram',   'tikhub_instagram_v2_fetch_post_info',              100, FALSE,  2000),
    ('instagram',   'tikhub_instagram_v3_get_post_info_by_code',        100, FALSE,  8000),
    ('instagram',   'tikhub_instagram_v3_get_post_info',                110, FALSE,  8000),

    ('youtube',     'tikhub_youtube_web_v2_get_video_info',             100, TRUE,   1000),
    ('youtube',     'tikhub_youtube_web_v2_get_video_info_v2',          110, TRUE,   1000),

    ('kuaishou',    'tikhub_kuaishou_app_fetch_one_video_by_url',       100, TRUE,   1000),
    ('kuaishou',    'tikhub_kuaishou_web_fetch_one_video_v2',           100, FALSE,  2000),
    ('kuaishou',    'tikhub_kuaishou_web_fetch_one_video_by_url',       110, FALSE,  2000),
    ('kuaishou',    'tikhub_kuaishou_web_fetch_one_video',              120, FALSE,  2000),

    ('wechat_mp',   'tikhub_wechat_mp_v2_fetch_article_detail',         100, TRUE,  10000)
) AS v(provider, slug, priority, enabled, price)
WHERE a.slug = 'tikhub'
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
DELETE FROM api_endpoints
WHERE slug LIKE 'tikhub\_%'
  AND account_id = (SELECT id FROM api_accounts WHERE slug = 'tikhub');
