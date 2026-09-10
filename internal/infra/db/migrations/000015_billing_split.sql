-- +goose Up
-- ADR-075 增值服务三块正交：基础订阅 / credits / 存储分开售卖，
-- 任何 plan 都不再附赠 credits 或存储。本迁移只动两处 schema，
-- 其余是代码层改造（档位门槛表、定价公式）。

-- ============ 1. credits 流水的幂等键 ============
-- 拆商品之前，重复发放的入口是订阅 webhook（Stripe 的 customer.subscription.updated
-- 在换卡、改 metadata 这类无关变更时同样触发，每次都重发一整月额度）。那条路径已随
-- CreditsByTier 一并删除，但幂等性缺口本身还在，且即将有两个新的调用方撞上它：
--   * 一次性支付回调（credits 加购）—— 支付网关重试是常态，不是异常；
--   * 管理后台手工发放 —— 客服重复提交表单，页面上写着「不可撤销」也拦不住。
--
-- 允许 NULL：运营调账这类一次性动作不需要幂等键，强制要求只会逼调用方随手编一个。
-- 因此用部分唯一索引而非列级 UNIQUE —— 二者对 NULL 的行为在 Postgres 里其实一致
-- （多个 NULL 互不冲突），但部分索引不为 NULL 行占空间，且把「只有非空才受约束」
-- 这个意图写在了索引定义里。
ALTER TABLE credits_ledger
    ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX uq_credits_ledger_idempotency
    ON credits_ledger(idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- ============ 2. 解析 endpoint 的加价倍率 ============
-- 此前用户侧售价是一个全局常量 SQUYRRL_PARSE_COST（默认 2 credits），而真实成本
-- 是逐 endpoint 的 unit_price_micros。二者脱钩的后果是：接入一个比常量贵的供应商，
-- 每次解析都在亏钱，且亏损随用量线性放大、账面上完全看不见。
--
-- 加价倍率用**万分比整数**而不是 NUMERIC：与同表的 unit_price_micros 一致，
-- 整个计价链路上没有一个浮点数，也就没有「$0.005 存成 0.004999999」这类问题。
-- 20000 bp = 2.0 倍 = 50% 毛利。
--
-- 售价公式（ADR-075，$1 = 10 000 credits ⇒ 1 credit = 100 micros）：
--   credit_cost = max(10, ceil(unit_price_micros × margin_bp / 1e6))
-- 下限 10 是给内置免费 provider（YouTube oEmbed / GenericOG）用的：它们上游成本为零，
-- 但仍然消耗 CPU、出网，并承担被目标站判定为爬虫的风险（ADR-048 的教训）。
ALTER TABLE api_endpoints
    ADD COLUMN margin_bp INT NOT NULL DEFAULT 20000
        CHECK (margin_bp > 0 AND margin_bp <= 1000000);

COMMENT ON COLUMN api_endpoints.margin_bp IS
    '加价倍率，万分比整数。20000 = 2.0 倍。售价 credits = ceil(unit_price_micros * margin_bp / 1e6)，下限 10。';

-- 注意：subscriptions 的唯一索引**刻意不动**。
-- uq_subscriptions_one_active_plan 只约束 kind='plan'，storage 因此可以有多行 active
-- —— 那正是 ADR-075 要的「存储可叠加购买」。存储按 $0.03/GB·月 线性定价，
-- 5 份 20 GB 与 1 份 100 GB 严格同价，叠加不会产生「买多份反而更贵」的陷阱，
-- 所以既不需要改成 per-kind 唯一，也不需要 proration 把多份合并到同一个到期日。

-- +goose Down
ALTER TABLE api_endpoints DROP COLUMN IF EXISTS margin_bp;
DROP INDEX IF EXISTS uq_credits_ledger_idempotency;
ALTER TABLE credits_ledger DROP COLUMN IF EXISTS idempotency_key;
