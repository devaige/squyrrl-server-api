-- +goose Up
-- =============================================================================
-- 作品本身不可见的记录（2026-10-21）。上游明确说作品因自身状态不可见（parser 的 errHidden，
-- 抖音 status_audit_self_see 等）时记一行，之后同一作品的普通解析直接回「不可见」，不再调上游。
--
-- 原因：失败不扣用户的费，上游却每次照收；客户端每重试一次，平台就为同一个结论再付一次钱。
-- 不设过期（用户决策）：测试反馈里这种状态几乎都是作品被平台下架，不会自己恢复。
-- 强制刷新（force）照常重取：取到内容即删掉这一行，再次确认不可见则刷新 confirmed_at。
-- 按作品记、不记用户：和 parse_cache 一样跨用户共享，A 确认过的，B 不必再付一次。
-- =============================================================================
CREATE TABLE parse_hidden (
    provider             TEXT        NOT NULL,
    provider_resource_id TEXT        NOT NULL,
    reason               TEXT        NOT NULL,
    confirmed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, provider_resource_id)
);

COMMENT ON TABLE parse_hidden IS
    '已确认作品本身不可见的资源。命中即回 422 reason=hidden、不调上游；force 重取到内容时删除。';
COMMENT ON COLUMN parse_hidden.reason IS
    '确认时的原因原文（含上游的过滤原因），只给后台排查看，不下发客户端。';

-- +goose Down
DROP TABLE IF EXISTS parse_hidden;
