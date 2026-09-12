-- +goose Up
-- GET /me/export 的每用户冷却期落点。
--
-- 这个端点是整套 API 里单次代价最高的一条：一次全表流式导出，上限是至尊档的
-- 1000 万条碎片（ADR-075）。一次调用 = 一次全量顺序扫描 + 等量的源站出网，
-- 而在这张表存在之前它没有任何节流 —— 没有冷却、没有并发限制、也不扣配额，
-- 重复打就是重复付钱，且打一半断开是最省事的打法。
--
-- **单开一张表而不是往 users 上加一列**：users 有 trg_users_updated_at 这个
-- BEFORE UPDATE 触发器，而 users.updated_at 是会读给客户端的字段（auth/repo.go）。
-- 加一列的后果是「点了一次导出」会表现为「账户资料被修改过」——
-- 一个将来一定会被当成同步 bug 去查的语义污染。
--
-- 行只在用户第一次导出时出现，所以这张表的行数是「导出过的人数」而不是用户数。
CREATE TABLE export_claims (
    user_id    UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    claimed_at TIMESTAMPTZ NOT NULL
);

COMMENT ON TABLE export_claims IS
    'GET /me/export 的冷却记录。claimed_at 是上一次被**受理**的时刻，不是完成时刻。';
COMMENT ON COLUMN export_claims.claimed_at IS
    '失败的导出同样占用名额：字节与全表扫描在失败之前就已经发生了。';

-- 不建额外索引：唯一的查询形态是按主键 user_id 取一行。

-- +goose Down
DROP TABLE IF EXISTS export_claims;
