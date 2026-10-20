-- +goose Up
-- GET /me/export 已删除（2026-10-20 用户决策：客户端不再提供数据导入导出）。
-- 这张表只记每个用户上一次导出的时刻，用于那个端点的冷却，没有别的读者。
DROP TABLE IF EXISTS export_claims;

-- +goose Down
CREATE TABLE export_claims (
    user_id    UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    claimed_at TIMESTAMPTZ NOT NULL
);

COMMENT ON TABLE export_claims IS
    'GET /me/export 的冷却记录。claimed_at 是上一次被**受理**的时刻，不是完成时刻。';
COMMENT ON COLUMN export_claims.claimed_at IS
    '失败的导出同样占用名额：字节与全表扫描在失败之前就已经发生了。';
