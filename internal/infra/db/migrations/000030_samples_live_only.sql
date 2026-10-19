-- +goose Up
-- =============================================================================
-- 用户决定 2026-10-19：「示例」改在代码里维护（parser/testdata/tikhub/ 的实测响应 +
-- parser/samples.go 的清单），部署即成为结构比对的基准；后台不再存为示例、手工录入、
-- 改备注或删除。原先基准存在这张表里，内置示例只在接口一份都没有时写一次，之后补进
-- 代码的形态到不了线上库。
--
-- 这张表从此只放线上采样：删掉 kind='sample' 的行（首次部署写入的内置示例，或在后台
-- 存下的副本），kind 与 note 两列随之没有用处，一并删除（kind 上的索引跟着列一起没了；
-- 清理与列表走 000009 的 idx_api_samples_endpoint_created）。
-- 这里在迁移里删数据，和 000019「不顺手删数据」并不冲突：删的正是被撤销的功能本身，
-- 而且不先删掉，示例行去掉 kind 之后就和线上采样分不开了。
-- =============================================================================
DELETE FROM api_samples WHERE kind = 'sample';
ALTER TABLE api_samples DROP COLUMN kind, DROP COLUMN note;

-- +goose Down
-- 删掉的示例行不恢复：down 只让旧代码能跑，旧代码启动时会把内置示例重新写进没有示例的接口。
ALTER TABLE api_samples
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'live' CHECK (kind IN ('sample','live')),
    ADD COLUMN note TEXT;
ALTER TABLE api_samples ALTER COLUMN kind SET DEFAULT 'sample';
CREATE INDEX idx_api_samples_endpoint_kind_created ON api_samples(endpoint_id, kind, created_at DESC);
