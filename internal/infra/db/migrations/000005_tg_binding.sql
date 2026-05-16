-- +goose Up
-- Telegram 账号绑定 = (tg_user_id) ↔ (user_id, device_id)
-- 为该绑定创建一个 platform=telegram 的设备占位

CREATE TABLE tg_binding_codes (
    code         TEXT        PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tg_binding_codes_user ON tg_binding_codes(user_id);

CREATE TABLE tg_bindings (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tg_user_id   BIGINT      NOT NULL UNIQUE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tg_bindings_user   ON tg_bindings(user_id);
CREATE INDEX idx_tg_bindings_device ON tg_bindings(device_id);


-- +goose Down
DROP TABLE IF EXISTS tg_bindings;
DROP TABLE IF EXISTS tg_binding_codes;
