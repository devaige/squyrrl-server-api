-- +goose Up
-- 绑定流程反转（用户决策 2026-09-06）：绑定码不再由 App 为已登录用户签发、
-- 拿到 Bot 里 /bind；改为 Bot 在收到未绑定用户的消息时为该 tg_user_id 签发，
-- 用户带着码到 App 内兑换。理由：这个功能的触发点几乎总在 TG 侧，让码从触发点
-- 产生，省掉「先切到 App 点一次按钮」的往返。
--
-- 整表重建而非 ALTER：(user_id, device_id) NOT NULL → tg_user_id NOT NULL 是
-- 语义反转而非字段增删，ALTER 路径要先允许空、回填、再收紧，比重建更绕。
-- 码只有 10 分钟 TTL，丢弃在途数据的代价仅是让正在绑定的人重发一条消息。

DROP TABLE IF EXISTS tg_binding_codes;

CREATE TABLE tg_binding_codes (
    code        TEXT        PRIMARY KEY,
    tg_user_id  BIGINT      NOT NULL,
    tg_username TEXT,
    tg_name     TEXT,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 部分索引，只覆盖未消费的码：同一 TG 用户在绑定前可能连发多条消息，
-- 每条都签发新码会让用户手上同时有一堆有效码，不知道该输哪个。
-- 有了这个索引就能「复用未过期的那张」，且索引只有活跃码那么大。
CREATE INDEX idx_tg_binding_codes_live ON tg_binding_codes(tg_user_id) WHERE consumed_at IS NULL;

-- App 的「已绑 TG 账号」列表要显示这是哪个号，否则用户面对一串数字 ID
-- 无从判断该解绑哪一个。绑定时由 Bot 带上，之后不再刷新（TG 侧改名不同步）。
ALTER TABLE tg_bindings ADD COLUMN IF NOT EXISTS tg_username TEXT;
ALTER TABLE tg_bindings ADD COLUMN IF NOT EXISTS tg_name     TEXT;

-- +goose Down
ALTER TABLE tg_bindings DROP COLUMN IF EXISTS tg_name;
ALTER TABLE tg_bindings DROP COLUMN IF EXISTS tg_username;

DROP TABLE IF EXISTS tg_binding_codes;

CREATE TABLE tg_binding_codes (
    code         TEXT        PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tg_binding_codes_user ON tg_binding_codes(user_id);
