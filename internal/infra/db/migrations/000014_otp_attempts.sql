-- +goose Up
-- OTP 猜解防护：给每条验证码加一个失败计数器。
--
-- 背景：验证码是 6 位数字（空间 10^6）、TTL 10 分钟，而 `/auth/email/verify` 既没有
-- 尝试次数限制、也没有速率限制 —— 免费版 Cloudflare 全 zone 只有一条限流规则，
-- 已经给了 `/auth/email/request`（ADR-074）。约 833 req/s 持续 10 分钟就能覆盖一半
-- 码空间；把速率压到 100 req/s，单个码周期仍有 ~6% 命中率，对着大量邮箱反复跑必然打穿。
--
-- 计数器把攻击成本从「速率」换成「必须重新触发发信」，而发信那一侧的三层防护
-- （冷却 / 按 IP 限流 / 全局日预算）已经把重新触发挡住了。这比在 verify 上再挂一条
-- 速率限制更根治：速率限制只是把同一次攻击拉长，猜解空间没有变小。
--
-- SMALLINT 足够：上限是个位数，达到即销毁该行，计数永远接近不了 32767。
ALTER TABLE email_otps
    ADD COLUMN attempts SMALLINT NOT NULL DEFAULT 0;

-- 刻意不为 attempts 加索引：失败计数的查询形状是 (email, purpose) + consumed_at IS NULL，
-- 既有的部分索引 idx_email_otps_lookup 已经覆盖；attempts 只出现在 SET 与 CASE 里，
-- 从不参与选择条件。

-- +goose Down
ALTER TABLE email_otps DROP COLUMN IF EXISTS attempts;
