-- +goose Up
-- 匿名数据 claim 幂等表：客户端每次发起 claim 时附带 client_dedupe_key（UUID），
-- 同一 key 重试 → 返回同一份 (user_id, snippet/page/tag/element 计数)，不重复写入。
-- ADR-051：匿名优先认证模型；登录后客户端把本地数据批量迁移到账户。
CREATE TABLE anonymous_claims (
    id              UUID        PRIMARY KEY,            -- = client_dedupe_key
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    snippet_count   INTEGER     NOT NULL DEFAULT 0,
    page_count      INTEGER     NOT NULL DEFAULT 0,
    tag_count       INTEGER     NOT NULL DEFAULT 0,
    element_count   INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_anonymous_claims_user ON anonymous_claims(user_id, created_at DESC);


-- +goose Down
DROP TABLE IF EXISTS anonymous_claims;
