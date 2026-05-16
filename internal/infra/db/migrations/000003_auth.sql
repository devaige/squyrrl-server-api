-- +goose Up
-- =============================================================================
-- 认证相关表：邮箱 OTP + Passkey 凭据
-- 邮箱 OTP 用于 Phase 1 登录；Passkey 表结构先建好，逻辑留待后续接入。
-- =============================================================================

-- 邮件 OTP（验证码登录）
-- 表中只存哈希；明文 OTP 仅在生成那一刻存在并通过邮件送达
CREATE TABLE email_otps (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       CITEXT      NOT NULL,
    code_hash   BYTEA       NOT NULL,                              -- SHA-256(明文 OTP)
    purpose     TEXT        NOT NULL CHECK (purpose IN ('login','add_email')),
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_email_otps_lookup
    ON email_otps(email, purpose, created_at DESC)
    WHERE consumed_at IS NULL;


-- WebAuthn / Passkey 凭据
CREATE TABLE passkeys (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id   BYTEA       NOT NULL UNIQUE,
    public_key      BYTEA       NOT NULL,
    sign_count      BIGINT      NOT NULL DEFAULT 0,
    transports      TEXT[]      NOT NULL DEFAULT '{}',             -- 'usb'/'nfc'/'ble'/'internal'/'hybrid'
    aaguid          UUID,
    name            TEXT,                                          -- 用户给该凭据起的名字
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ
);
CREATE INDEX idx_passkeys_user ON passkeys(user_id);


-- +goose Down
DROP TABLE IF EXISTS passkeys;
DROP TABLE IF EXISTS email_otps;
