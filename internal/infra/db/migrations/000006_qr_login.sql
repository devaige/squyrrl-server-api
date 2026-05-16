-- +goose Up
-- 已登录设备签发的一次性绑定码，新设备扫码或粘贴后兑换 token
CREATE TABLE qr_login_codes (
    code         TEXT        PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_qr_login_codes_user ON qr_login_codes(user_id);


-- +goose Down
DROP TABLE IF EXISTS qr_login_codes;
