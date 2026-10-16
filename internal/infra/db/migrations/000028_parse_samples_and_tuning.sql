-- +goose Up
-- =============================================================================
-- 用户决定 2026-10-16，接在 000027 之后：
-- 1. 撤销「结果不完整时继续试下一个接口」—— 每次解析都可能多花钱。小红书改回视频接口在前、
--    图文接口在后：没有 type 的链接（网页端复制）先试视频接口，是图文笔记再走图文接口，
--    这类链接按两倍计费（代码里的 PriceFactorer）。000027 加进约束的 'partial' 不再产生，
--    约束里留着它，已有的记录仍然合法。
-- 2. Instagram 停用 v1 按 ID：它总是和 v1 按链接一起失败，只多花钱、不多成功（v2 已在 000027 启用）。
-- 3. 解析记录的每一步关联当次调用的线上采样（请求与原始响应），排查时可以直接点开，
--    发现新结构就把它存为示例。采样按保留期清理后这里置空。
-- =============================================================================
UPDATE api_endpoints SET priority = 100 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_video_note_detail';
UPDATE api_endpoints SET priority = 110 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_image_note_detail';

UPDATE api_endpoints SET enabled = FALSE WHERE slug = 'tikhub_instagram_v1_fetch_post_by_id';

ALTER TABLE parse_attempts ADD COLUMN sample_id UUID REFERENCES api_samples(id) ON DELETE SET NULL;

-- 接口详情页按「示例 / 线上采样」分开列，清理按接口 + 时间扫
CREATE INDEX idx_api_samples_endpoint_kind_created ON api_samples(endpoint_id, kind, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_api_samples_endpoint_kind_created;
ALTER TABLE parse_attempts DROP COLUMN sample_id;
UPDATE api_endpoints SET enabled = TRUE WHERE slug = 'tikhub_instagram_v1_fetch_post_by_id';
UPDATE api_endpoints SET priority = 100 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_image_note_detail';
UPDATE api_endpoints SET priority = 110 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_video_note_detail';
