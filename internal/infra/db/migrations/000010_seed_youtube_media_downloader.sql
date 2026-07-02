-- +goose Up
-- =============================================================================
-- Seed：接入 RapidAPI「YouTube Media Downloader」(DataFanatic) 的 video/details 端点
--   host     : youtube-media-downloader.p.rapidapi.com
--   endpoint : GET /v2/video/details?videoId=<id>
--   鉴权     : x-rapidapi-key（走 ${env:SQUYRRL_RAPIDAPI_KEY}）+ x-rapidapi-host
--
-- 因 extapi 的 driver 是配置驱动的（ADR-056），接入一个 REST endpoint = 插一行数据，无需 Go 代码。
--
-- 故意 enabled=FALSE + unit_price=0：先落库但不启用，避免在你于后台录入单价/额度前就产生
-- 「花了 RapidAPI 真钱却没记平台成本」的情况。启用清单见文件末尾注释。
--
-- response_map 说明：
--   title (=title)、thumbnail_url (=thumbnails.0.url) —— 已由封装库 struct 确认。
--   author_name (=channel.name) —— **未确认**：该 API 不同版本作者字段有嵌套 channel.name 与
--       扁平 author 两种形态，公开文档无法定论，此处取 v2 最可能的 channel.name。
--   author_url —— 暂空（channel 对象的 URL/handle 字段名同样待确认）。
--   description —— 最佳推断。
-- config.archive_live=true → 首次真实调用把完整响应存进 api_samples；据此在后台核对：
--   若作者其实是扁平 author，把 author_name 改为 "author" 即可（改配置，不改代码）。
-- =============================================================================

INSERT INTO api_endpoints (provider, vendor, slug, priority, enabled, unit_price_micros, currency, config)
VALUES (
    'youtube',
    'rapidapi',
    'rapidapi_youtube_media_downloader_video_details',
    50,
    FALSE,
    0,
    'USD',
    '{
        "method": "GET",
        "url_template": "https://youtube-media-downloader.p.rapidapi.com/v2/video/details?videoId=${resource_id}",
        "headers": {
            "x-rapidapi-key": "${env:SQUYRRL_RAPIDAPI_KEY}",
            "x-rapidapi-host": "youtube-media-downloader.p.rapidapi.com"
        },
        "response_map": {
            "title": "title",
            "description": "description",
            "thumbnail_url": "thumbnails.0.url",
            "author_name": "channel.name",
            "author_url": ""
        },
        "archive_live": true,
        "timeout_ms": 8000,
        "meta": {
            "vendor_url": "https://rapidapi.com/user/DataFanatic",
            "api_page": "https://rapidapi.com/DataFanatic/api/youtube-media-downloader",
            "endpoint": "GET /v2/video/details"
        }
    }'::jsonb
)
ON CONFLICT (slug) DO NOTHING;

-- 启用清单（在管理后台「解析 API」完成）：
--   1) 在 API 服务环境设置 SQUYRRL_RAPIDAPI_KEY
--   2) 录入单价（unit_price）与充值额度（topup / 余额）
--   3) 用一条 YouTube 链接测试解析 → 打开该 endpoint 的存档，核对 response_map 是否命中
--   4) 确认无误后「启用」

-- +goose Down
DELETE FROM api_endpoints WHERE slug = 'rapidapi_youtube_media_downloader_video_details';
