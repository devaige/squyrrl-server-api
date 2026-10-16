-- +goose Up
-- =============================================================================
-- 解析上游重构（ADR-080）
--
-- 1. api_accounts：计费账户，即「钱实际存放、被扣减的地方」。
--    上游只有两种形态：TikHub 这类直连上游 = 一个账户，所有接口共用一份余额；
--    RapidAPI 这类聚合平台 = 每个子提供商一个账户（各是一份独立订阅）。
--    把余额挂在账户上，两种形态就只差「一个上游下有几个账户」。
-- 2. api_endpoints 归属账户。vendor / currency 随之移到账户上；margin_bp 废弃 ——
--    售价改为按平台固定（parse_prices），不再由单个接口的单价 × 倍率决定。
--    config 只剩适配器运行参数（timeout_ms / archive_live），请求与映射改由
--    Go 适配器按 slug 实现（parser.Adapter），原 url_template / response_map 不再读取。
-- 3. api_cost_ledger 按账户记账；endpoint_id 变为可空（充值不属于任何接口），
--    删接口也不再连带抹掉账。
-- 4. parse_prices：按平台的固定售价覆盖值。无行 = 自动价（该平台已启用接口的最高单价 × 2）。
-- 5. parse_logs / parse_attempts：每次解析、每次上游尝试各一行，用于核对成本收益与排查失败。
--    **不记用户身份**。与 parse_cache 无关：缓存给用户返回内容，记录给管理后台对账。
-- =============================================================================

-- ============ 计费账户 ============
CREATE TABLE api_accounts (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor     TEXT        NOT NULL,                 -- 'tikhub' / 'rapidapi'：决定鉴权方式与响应外壳（extapi.vendors）
    slug       TEXT        NOT NULL UNIQUE,
    name       TEXT        NOT NULL,                 -- 后台展示名，如「RapidAPI · YouTube Media Downloader」
    currency   TEXT        NOT NULL DEFAULT 'USD',
    note       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_api_accounts_updated_at
    BEFORE UPDATE ON api_accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 回填：现存 endpoint 各自成为一个账户。它们此前就各记各的余额，一一对应不丢账。
INSERT INTO api_accounts (vendor, slug, name, currency)
SELECT vendor, slug, vendor || ' · ' || slug, currency FROM api_endpoints;

-- TikHub 是一个账户、所有接口共用余额，先建好，后台可以直接录充值。
INSERT INTO api_accounts (vendor, slug, name) VALUES ('tikhub', 'tikhub', 'TikHub')
ON CONFLICT (slug) DO NOTHING;

-- ============ 接口归属账户 ============
ALTER TABLE api_endpoints ADD COLUMN account_id UUID REFERENCES api_accounts(id) ON DELETE RESTRICT;
UPDATE api_endpoints e SET account_id = a.id FROM api_accounts a WHERE a.slug = e.slug;
ALTER TABLE api_endpoints ALTER COLUMN account_id SET NOT NULL;

ALTER TABLE api_endpoints DROP COLUMN vendor;
ALTER TABLE api_endpoints DROP COLUMN currency;
ALTER TABLE api_endpoints DROP COLUMN margin_bp;

-- 兜底链顺序改为单价升序（同价再看 priority），索引跟着换。
DROP INDEX IF EXISTS idx_api_endpoints_provider_priority;
CREATE INDEX idx_api_endpoints_provider_price
    ON api_endpoints(provider, unit_price_micros, priority)
    WHERE enabled;

-- ============ 成本账按账户记 ============
ALTER TABLE api_cost_ledger ADD COLUMN account_id UUID REFERENCES api_accounts(id) ON DELETE CASCADE;
UPDATE api_cost_ledger l SET account_id = e.account_id FROM api_endpoints e WHERE e.id = l.endpoint_id;
ALTER TABLE api_cost_ledger ALTER COLUMN account_id SET NOT NULL;

ALTER TABLE api_cost_ledger ALTER COLUMN endpoint_id DROP NOT NULL;
ALTER TABLE api_cost_ledger DROP CONSTRAINT api_cost_ledger_endpoint_id_fkey;
ALTER TABLE api_cost_ledger ADD CONSTRAINT api_cost_ledger_endpoint_id_fkey
    FOREIGN KEY (endpoint_id) REFERENCES api_endpoints(id) ON DELETE SET NULL;

-- 充值备注：支付渠道、订单号、套餐名……核对时要能对上上游后台的那一笔。
ALTER TABLE api_cost_ledger ADD COLUMN note TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_api_cost_ledger_account_created
    ON api_cost_ledger(account_id, created_at DESC);

-- ============ 按平台的固定售价 ============
CREATE TABLE parse_prices (
    provider   TEXT        PRIMARY KEY,              -- 平台，对应 Parser.Provider()
    credits    BIGINT      NOT NULL CHECK (credits > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_parse_prices_updated_at
    BEFORE UPDATE ON parse_prices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============ 解析记录 ============
CREATE TABLE parse_logs (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider        TEXT        NOT NULL,
    resource_id     TEXT        NOT NULL,
    -- ok：返回了内容并收了钱；failed：整条链没拿到内容，不收钱；
    -- unpaid：拿到了内容（成本已发生），结算时余额已被并发请求花掉，未收到钱。
    status          TEXT        NOT NULL CHECK (status IN ('ok', 'failed', 'unpaid')),
    cached          BOOLEAN     NOT NULL DEFAULT FALSE,
    shared          BOOLEAN     NOT NULL DEFAULT FALSE,  -- 并发合并：搭了同资源另一请求的上游调用，自身无成本
    forced          BOOLEAN     NOT NULL DEFAULT FALSE,
    credits_charged BIGINT      NOT NULL DEFAULT 0,      -- 收益
    cost_micros     BIGINT      NOT NULL DEFAULT 0,      -- 成本：本次所有上游尝试之和
    source          TEXT        NOT NULL DEFAULT '',     -- 产出内容的来源：builtin:<平台> 或接口 slug；命中缓存为空
    error           TEXT        NOT NULL DEFAULT '',
    latency_ms      INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_parse_logs_created ON parse_logs(created_at DESC, id DESC);
CREATE INDEX idx_parse_logs_provider_created ON parse_logs(provider, created_at DESC);

CREATE TABLE parse_attempts (
    id                   UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    log_id               UUID    NOT NULL REFERENCES parse_logs(id) ON DELETE CASCADE,
    seq                  INT     NOT NULL,
    source               TEXT    NOT NULL,
    endpoint_id          UUID    REFERENCES api_endpoints(id) ON DELETE SET NULL,
    account_id           UUID    REFERENCES api_accounts(id) ON DELETE SET NULL,
    outcome              TEXT    NOT NULL CHECK (outcome IN ('ok', 'failed', 'skipped')),
    error_kind           TEXT    NOT NULL DEFAULT '',
    error                TEXT    NOT NULL DEFAULT '',
    http_status          INT     NOT NULL DEFAULT 0,
    latency_ms           INT     NOT NULL DEFAULT 0,
    cost_micros          BIGINT  NOT NULL DEFAULT 0,
    balance_after_micros BIGINT,                         -- 付费尝试后该账户的余额快照；内置来源为空
    UNIQUE (log_id, seq)
);
CREATE INDEX idx_parse_attempts_account ON parse_attempts(account_id) WHERE account_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS parse_attempts;
DROP TABLE IF EXISTS parse_logs;
DROP TABLE IF EXISTS parse_prices;

DROP INDEX IF EXISTS idx_api_cost_ledger_account_created;
ALTER TABLE api_cost_ledger DROP COLUMN note;
-- 账户级的流水（充值）不属于任何接口，回到旧结构只能舍弃。
DELETE FROM api_cost_ledger WHERE endpoint_id IS NULL;
ALTER TABLE api_cost_ledger DROP CONSTRAINT api_cost_ledger_endpoint_id_fkey;
ALTER TABLE api_cost_ledger ADD CONSTRAINT api_cost_ledger_endpoint_id_fkey
    FOREIGN KEY (endpoint_id) REFERENCES api_endpoints(id) ON DELETE CASCADE;
ALTER TABLE api_cost_ledger ALTER COLUMN endpoint_id SET NOT NULL;
ALTER TABLE api_cost_ledger DROP COLUMN account_id;

DROP INDEX IF EXISTS idx_api_endpoints_provider_price;
ALTER TABLE api_endpoints ADD COLUMN vendor TEXT;
ALTER TABLE api_endpoints ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE api_endpoints ADD COLUMN margin_bp INT NOT NULL DEFAULT 20000
    CHECK (margin_bp > 0 AND margin_bp <= 1000000);
UPDATE api_endpoints e SET vendor = a.vendor, currency = a.currency FROM api_accounts a WHERE a.id = e.account_id;
ALTER TABLE api_endpoints ALTER COLUMN vendor SET NOT NULL;
ALTER TABLE api_endpoints DROP COLUMN account_id;
CREATE INDEX idx_api_endpoints_provider_priority
    ON api_endpoints(provider, priority)
    WHERE enabled;

DROP TABLE IF EXISTS api_accounts;
