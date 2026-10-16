-- +goose Up
-- =============================================================================
-- 1. 尝试结果新增 partial：结果能用但缺东西（典型：视频作品拿不到视频文件）。
--    兜底链会继续试后面的来源，都失败时用这份兜底，而不是整条解析失败。
-- 2. Instagram 启用 v2（$0.002，用户决定 2026-10-16）：v1 会对部分存在的帖子误报「不存在」，
--    v2 兜底保成功率。平台自动售价随之从 20 变为 40 代币（最高单价 × 2）。
-- 3. 小红书图文接口排到视频接口前面：没有 type 的链接先走图文，是视频笔记时记为不完整，
--    再走视频接口拿视频文件（见 parser/tikhub.go 的分流说明）。
-- =============================================================================
ALTER TABLE parse_attempts DROP CONSTRAINT parse_attempts_outcome_check;
ALTER TABLE parse_attempts ADD CONSTRAINT parse_attempts_outcome_check
    CHECK (outcome IN ('ok', 'failed', 'skipped', 'partial'));

UPDATE api_endpoints SET enabled = TRUE WHERE slug = 'tikhub_instagram_v2_fetch_post_info';

UPDATE api_endpoints SET priority = 100 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_image_note_detail';
UPDATE api_endpoints SET priority = 110 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_video_note_detail';

-- +goose Down
UPDATE api_endpoints SET priority = 100 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_video_note_detail';
UPDATE api_endpoints SET priority = 110 WHERE slug = 'tikhub_xiaohongshu_app_v2_get_image_note_detail';
UPDATE api_endpoints SET enabled = FALSE WHERE slug = 'tikhub_instagram_v2_fetch_post_info';

UPDATE parse_attempts SET outcome = 'failed' WHERE outcome = 'partial';
ALTER TABLE parse_attempts DROP CONSTRAINT parse_attempts_outcome_check;
ALTER TABLE parse_attempts ADD CONSTRAINT parse_attempts_outcome_check
    CHECK (outcome IN ('ok', 'failed', 'skipped'));
