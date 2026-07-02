-- +goose Up
-- =============================================================================
-- 第三方解析 API 供应层
-- special 类解析器（youtube/twitter/reddit…）可路由到多个外部付费 API endpoint。
-- 同一 provider 下的多个 endpoint 按 priority 升序构成兜底链：前一个不可用则试下一个，
-- 直到成功或全部耗尽（此时上层回退到内置免费实现，如 YouTube oEmbed）。
-- 供应商无关：vendor 只是分组/展示标签（rapidapi/apify/…），新接入 = 新增一行 + 填 config。
-- =============================================================================

-- ============ endpoint 目录 ============
CREATE TABLE api_endpoints (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          TEXT        NOT NULL,                   -- 对应 Parser.Provider()：'youtube'/'twitter'/'reddit'…
    vendor            TEXT        NOT NULL,                   -- 供应商标签：'rapidapi'/'apify'/…（仅分组/展示）
    slug              TEXT        NOT NULL,                   -- 唯一稳定键，driver 与之绑定，如 'rapidapi_youtube_v2'
    priority          INT         NOT NULL DEFAULT 100,       -- 兜底链顺序：升序优先，越小越先尝试
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    unit_price_micros BIGINT      NOT NULL DEFAULT 0,         -- 每次成功调用成本，单位=币种的百万分之一（整数存钱，杜绝浮点误差）
    currency          TEXT        NOT NULL DEFAULT 'USD',
    config            JSONB       NOT NULL DEFAULT '{}',      -- 调用配置：method/url_template/headers/query/response_map/archive_live/timeout_ms
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (slug)
);
-- 兜底链查询：按 provider 取 enabled 的 endpoint，priority 升序
CREATE INDEX idx_api_endpoints_provider_priority
    ON api_endpoints(provider, priority)
    WHERE enabled;
CREATE TRIGGER trg_api_endpoints_updated_at
    BEFORE UPDATE ON api_endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============ 平台侧成本账 ============
-- 镜像 credits_ledger 的设计：纯追加，余额 = SUM(delta)，无需维护可变余额列。
-- delta 正 = 充值/额度录入；负 = 一次成功调用扣费（= 当时的 unit_price）。
CREATE TABLE api_cost_ledger (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id UUID        NOT NULL REFERENCES api_endpoints(id) ON DELETE CASCADE,
    delta       BIGINT      NOT NULL,                         -- micros；正=充值，负=调用成本
    reason      TEXT        NOT NULL,                         -- 'topup'/'call'/'adjust'
    resource_id TEXT,                                         -- 调用时记录被解析资源（视频 ID 等），便于回溯对账
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_api_cost_ledger_endpoint_created
    ON api_cost_ledger(endpoint_id, created_at DESC);

-- ============ 请求/响应存档 ============
-- kind='sample'：管理员录入的请求响应示例（点 3 的核心，用于回溯 endpoint 契约）。
-- kind='live'  ：线上真实调用的采样（endpoint.config.archive_live=true 时写入）。
-- request 中的 headers 只存未解析的模板（如 "${env:SQUYRRL_RAPIDAPI_KEY}"），密钥永不落库。
CREATE TABLE api_samples (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id UUID        NOT NULL REFERENCES api_endpoints(id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL DEFAULT 'sample' CHECK (kind IN ('sample','live')),
    resource_id TEXT,
    request     JSONB,                                        -- {method,url,headers(脱敏),query,body}
    response    JSONB,                                        -- {status,body}
    http_status INT,
    latency_ms  INT,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_api_samples_endpoint_created
    ON api_samples(endpoint_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS api_samples;
DROP TABLE IF EXISTS api_cost_ledger;
DROP TABLE IF EXISTS api_endpoints;
