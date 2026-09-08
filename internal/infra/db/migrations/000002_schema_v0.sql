-- +goose Up
-- =============================================================================
-- Schema v0
-- 用户 / 设备 / 会话 / 文件 / 页面 / 标签 / 碎片 / 元素 / 解析缓存 / 订阅 / 代币
-- 设计依据：docs/memory.md ADR-001 至 ADR-013
-- =============================================================================

-- 通用 updated_at 自动维护触发器
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd


-- ============ 用户 ============
CREATE TABLE users (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       CITEXT      UNIQUE,
    handle      TEXT        UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 设备 ============
-- 用户在不同终端的登录实体；同时承担"手动碎片来源"中"用户可读设备名"的角色
CREATE TABLE devices (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT        NOT NULL,                          -- "iPhone 17 Pro Max" / "Pixel 8a" / "MacBook"
    platform        TEXT        NOT NULL,                          -- ios|android|macos|windows|linux|web|browser|telegram|wechat
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ                                    -- 设备数超限被踢时设置（ADR-008）
);
CREATE INDEX idx_devices_user_active
    ON devices(user_id)
    WHERE revoked_at IS NULL;
CREATE TRIGGER trg_devices_updated_at
    BEFORE UPDATE ON devices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 会话 ============
-- 一个会话 = 一个登录态。token 仅以 SHA-256 哈希入库，避免数据库泄露后被滥用
CREATE TABLE sessions (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id           UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    access_token_hash   BYTEA       NOT NULL,
    refresh_token_hash  BYTEA       NOT NULL,
    access_expires_at   TIMESTAMPTZ NOT NULL,
    refresh_expires_at  TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at          TIMESTAMPTZ
);
CREATE INDEX idx_sessions_access_token
    ON sessions(access_token_hash)
    WHERE revoked_at IS NULL;
CREATE INDEX idx_sessions_user_device
    ON sessions(user_id, device_id);


-- ============ 文件（物理 blob） ============
-- 跨用户共享去重。详见 ADR-004。
CREATE TABLE files (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    plain_hash      BYTEA       NOT NULL UNIQUE,                   -- 原始字节 SHA-256：去重键
    cipher_hash     BYTEA       NOT NULL,                          -- 混淆字节 SHA-256：对象存储 key
    size_bytes      BIGINT      NOT NULL,                          -- 原始字节大小，用于配额
    mime            TEXT        NOT NULL DEFAULT 'application/octet-stream',
    storage_bucket  TEXT        NOT NULL,
    storage_key     TEXT        NOT NULL,                          -- `blob/<cipher_hash 的 hex>`；前缀见 file.StorageKeyFor
    ref_count       INT         NOT NULL DEFAULT 0,                -- 引用计数；归零后由 GC 删除
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);


-- ============ 页面 ============
-- snippet.page_id 可为 NULL（详见 ADR-007："全部碎片"虚拟视图）
CREATE TABLE pages (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    is_hidden   BOOLEAN     NOT NULL DEFAULT FALSE,                -- 隐藏页面：付费功能
    is_system   BOOLEAN     NOT NULL DEFAULT FALSE,                -- 例如"冲突收件箱"，系统创建不可删
    order_idx   INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE INDEX idx_pages_user_active
    ON pages(user_id)
    WHERE deleted_at IS NULL;
CREATE TRIGGER trg_pages_updated_at
    BEFORE UPDATE ON pages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 标签 ============
CREATE TABLE tags (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
CREATE TRIGGER trg_tags_updated_at
    BEFORE UPDATE ON tags
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 碎片 ============
-- 类型层级：ADR-001  | 文本/文件混淆：ADR-003  | 同步并发控制：ADR-002
CREATE TABLE snippets (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    page_id         UUID        REFERENCES pages(id) ON DELETE SET NULL,

    type            TEXT        NOT NULL CHECK (type IN ('uri','url','normal','special')),
    subtype         TEXT,                                          -- type=special 时具体子类（'tweet'/'apk'/'gist'/'youtube'/...）

    title           TEXT,
    description     TEXT,

    text_content    BYTEA,                                         -- 混淆字节
    text_format     TEXT        NOT NULL DEFAULT 'plain' CHECK (text_format IN ('plain','markdown','code')),
    text_lang       TEXT,                                          -- 仅 text_format=code 时使用

    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb,      -- 类型相关字段：原始 URI、URL、特殊子类的解析数据等

    -- 来源信息内嵌（详见 docs/memory.md ADR-016）
    source_type     TEXT        NOT NULL CHECK (source_type IN ('device','upstream')),
    source_data     JSONB       NOT NULL DEFAULT '{}'::jsonb,      -- device: {device_id,name,platform}；upstream: {provider,author_handle,author_avatar_url,...}

    version         BIGINT      NOT NULL DEFAULT 1,                -- 写入时单调递增；同步乐观锁

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX idx_snippets_user_active
    ON snippets(user_id)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_snippets_user_page
    ON snippets(user_id, page_id)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_snippets_user_updated
    ON snippets(user_id, updated_at);                              -- 增量同步拉取
CREATE INDEX idx_snippets_subtype
    ON snippets(subtype)
    WHERE subtype IS NOT NULL;
CREATE TRIGGER trg_snippets_updated_at
    BEFORE UPDATE ON snippets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 元素（碎片对文件的引用） ============
CREATE TABLE elements (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    snippet_id  UUID        NOT NULL REFERENCES snippets(id) ON DELETE CASCADE,
    file_id     UUID        NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    order_idx   INT         NOT NULL DEFAULT 0,
    alt_text    TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_elements_snippet ON elements(snippet_id);
CREATE INDEX idx_elements_file    ON elements(file_id);


-- ============ 碎片标签关联 ============
CREATE TABLE snippet_tags (
    snippet_id  UUID        NOT NULL REFERENCES snippets(id) ON DELETE CASCADE,
    tag_id      UUID        NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (snippet_id, tag_id)
);
CREATE INDEX idx_snippet_tags_tag ON snippet_tags(tag_id);


-- ============ URI 解析缓存 ============
-- ADR-005：跨用户复用解析结果
CREATE TABLE parse_cache (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider                TEXT        NOT NULL,                  -- 'twitter'/'github_gist'/'youtube'/...
    provider_resource_id    TEXT        NOT NULL,                  -- 推文 ID、gist ID、视频 ID 等
    payload                 JSONB       NOT NULL,                  -- 解析得到的结构化数据
    file_ids                UUID[]      NOT NULL DEFAULT '{}',     -- 解析过程中入库的文件
    expires_at              TIMESTAMPTZ,                           -- 软过期；NULL 表示永不过期
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_resource_id)
);


-- ============ 订阅（会员 + 存储） ============
-- 会员（plan）同一时间只能有一条 active；存储（storage）可叠加多条 active
CREATE TABLE subscriptions (
    id                                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                             UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind                                TEXT        NOT NULL CHECK (kind IN ('plan','storage')),
    tier                                TEXT        NOT NULL,      -- plan: free/basic/standard/premium/maximum；storage: 10gb/20gb/...
    bonus_storage_gb                    INT,                       -- storage 类型时填入增量空间（含赠送）
    billing_period                      TEXT        NOT NULL CHECK (billing_period IN ('monthly','yearly')),
    status                              TEXT        NOT NULL CHECK (status IN ('active','past_due','canceled','expired')),
    current_period_start                TIMESTAMPTZ NOT NULL,
    current_period_end                  TIMESTAMPTZ NOT NULL,
    payment_provider                    TEXT        NOT NULL,      -- 'app_store'/'google_play'/'stripe'/'wechat_pay'/'alipay'/...
    payment_provider_subscription_id    TEXT,
    canceled_at                         TIMESTAMPTZ,
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_subscriptions_user_active
    ON subscriptions(user_id, kind)
    WHERE status = 'active';
-- 同一用户在 active 状态下最多一条 plan 订阅
CREATE UNIQUE INDEX uq_subscriptions_one_active_plan
    ON subscriptions(user_id)
    WHERE kind = 'plan' AND status = 'active';
CREATE TRIGGER trg_subscriptions_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ============ 代币流水 ============
-- 仅追加表；当前余额 = 该用户最新一行的 balance_after
CREATE TABLE credits_ledger (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    delta           BIGINT      NOT NULL,                          -- 正数=入账（购买/赠送）；负数=消费
    balance_after   BIGINT      NOT NULL,
    reason          TEXT        NOT NULL,                          -- 'purchase'/'parse_uri'/'archive_url'/'admin_grant'/...
    ref_type        TEXT,                                          -- 'snippet'/'payment'/...
    ref_id          UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_credits_ledger_user_created
    ON credits_ledger(user_id, created_at DESC);


-- +goose Down
DROP TABLE IF EXISTS credits_ledger;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS parse_cache;
DROP TABLE IF EXISTS snippet_tags;
DROP TABLE IF EXISTS elements;
DROP TABLE IF EXISTS snippets;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS pages;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS users;
DROP FUNCTION IF EXISTS set_updated_at();
