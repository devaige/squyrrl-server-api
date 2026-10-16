-- +goose Up
-- =============================================================================
-- 第二批 TikHub 接口（ADR-080，2026-10-16）：知乎、Threads、Reddit、视频号、头条（含西瓜视频）、
-- Lemon8、皮皮虾、LinkedIn、Telegram 公开频道，只接「取单条作品」的接口。
--
-- 每行对应 parser/tikhub2.go 里的一个适配器（slug 相同），单价取自 TikHub
-- get_all_endpoints_info 的实时报价（美元 micros）。没有手动设定售价时 = 已启用接口的最高单价 × 2：
-- Threads 40 代币，其余 20；Reddit 原先只有内置免费来源（默认 10），接上付费兜底后变成 20。
--
-- 分流（同价时看 priority，各接口的 Accept 决定接不接这条链接）：
--   - 知乎四种内容各一个接口，按链接类型（回答 / 专栏 / 想法 / 问题）各走各的；
--   - 头条：文章与微头条走网页版文章接口；视频先网页版，再 App 版（App 版对新视频常不给播放地址）；
--     看不出类型的 group 链接先试文章接口，是视频再交给视频接口；
--   - Threads：v2 收 shortcode 在前，v1 要换算成数字 ID 在后。
--
-- 视频号默认不启用：视频文件是加密的（前 128 KB 要用 decode_key 解密），客户端只能存下封面和文字，
-- 而单价 $0.01 意味着售价 200 代币。要上线请在后台启用。
-- 西瓜视频自己的两个接口不接（v1 没有标题、只有 720p，v2 报错），西瓜链接由头条视频接口处理。
-- =============================================================================

INSERT INTO api_endpoints (account_id, provider, slug, priority, enabled, unit_price_micros, config)
SELECT a.id, v.provider, v.slug, v.priority, v.enabled, v.price, '{"archive_live": true}'::jsonb
FROM api_accounts a
CROSS JOIN (VALUES
    ('zhihu',           'tikhub_zhihu_web_fetch_answer_detail',           100, TRUE,   1000),
    ('zhihu',           'tikhub_zhihu_web_fetch_column_article_detail',   100, TRUE,   1000),
    ('zhihu',           'tikhub_zhihu_web_fetch_pin_detail',              100, TRUE,   1000),
    ('zhihu',           'tikhub_zhihu_web_fetch_question_detail',         100, TRUE,   1000),

    ('threads',         'tikhub_threads_web_fetch_post_detail_v2',        100, TRUE,   2000),
    ('threads',         'tikhub_threads_web_fetch_post_detail',           110, TRUE,   2000),

    ('reddit',          'tikhub_reddit_app_fetch_post_details',           100, TRUE,   1000),

    ('wechat_channels', 'tikhub_wechat_channels_v2_fetch_video_detail',   100, FALSE, 10000),

    ('toutiao',         'tikhub_toutiao_web_get_article_info',            100, TRUE,   1000),
    ('toutiao',         'tikhub_toutiao_web_get_video_info',              110, TRUE,   1000),
    ('toutiao',         'tikhub_toutiao_app_get_video_info',              120, TRUE,   1000),

    ('lemon8',          'tikhub_lemon8_app_fetch_post_detail',            100, TRUE,   1000),

    ('pipixia',         'tikhub_pipixia_app_fetch_post_detail',           100, TRUE,   1000),

    ('linkedin',        'tikhub_linkedin_web_v2_get_post_detail',         100, TRUE,   1000),

    ('telegram',        'tikhub_telegram_web_fetch_post_detail',          100, TRUE,   1000)
) AS v(provider, slug, priority, enabled, price)
WHERE a.slug = 'tikhub'
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
DELETE FROM api_endpoints
WHERE slug IN (
    'tikhub_zhihu_web_fetch_answer_detail', 'tikhub_zhihu_web_fetch_column_article_detail',
    'tikhub_zhihu_web_fetch_pin_detail', 'tikhub_zhihu_web_fetch_question_detail',
    'tikhub_threads_web_fetch_post_detail_v2', 'tikhub_threads_web_fetch_post_detail',
    'tikhub_reddit_app_fetch_post_details', 'tikhub_wechat_channels_v2_fetch_video_detail',
    'tikhub_toutiao_web_get_article_info', 'tikhub_toutiao_web_get_video_info', 'tikhub_toutiao_app_get_video_info',
    'tikhub_lemon8_app_fetch_post_detail', 'tikhub_pipixia_app_fetch_post_detail',
    'tikhub_linkedin_web_v2_get_post_detail', 'tikhub_telegram_web_fetch_post_detail'
);
