-- +goose Up
-- 绑定载体改为 Telegram deep link（用户决策 2026-09-06）。
--
-- 000011 让 Bot 为未绑定的 tg_user_id 签发一个 8 位码、用户到 App 里手输兑换。
-- 现在方向调回「App 签发 → TG 核销」，但载体不再是要人搬运的码，而是
-- t.me/<bot>?start=<token>：App 出链接 + 二维码，用户点一下就把 token 带进 Bot。
--
-- 换方向的实际动因是计费：文件摄取将成为付费功能，配额是花钱买的。
-- 「Bot 出码」方向下，码代表 TG 号，泄漏只会让别人把你的 TG 抓进他自己的库，
-- 烧的是他的额度；而「App 出令牌」方向下令牌代表 Squyrrl 账户，泄漏等于让人
-- 往你的库里灌文件、烧你买的配额 —— 风险朝向对用户不利。deep link 把令牌从
-- 「一串人眼可读、会被顺手粘进群的码」变成一次性链接，正是为了收窄这个面。
--
-- 表整个换名而非改列：codes → tokens 是载体语义的更替，留着旧名字会让
-- 「code 是给人读的」这个已经不成立的暗示继续误导后来者。
DROP TABLE IF EXISTS tg_binding_codes;

CREATE TABLE tg_binding_tokens (
    token       TEXT        PRIMARY KEY,          -- base64url(32B)，43 字符，落在 deep link payload 的 64 字符与字符集限制内
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 部分索引只覆盖未核销的令牌：绑定页每次重建都会问一次「当前令牌」，
-- 有这个索引就能复用未过期的那一张，二维码不会在用户扫的过程中变掉。
CREATE INDEX idx_tg_binding_tokens_live ON tg_binding_tokens(user_id) WHERE consumed_at IS NULL;

-- 注意：绑定不再预先关联 device。设备在 Bot 核销令牌时才创建 —— 那一刻才知道
-- 是哪个 TG 号，设备名里才带得上 @username；签发即建设备会给从未使用的令牌
-- 留下一堆空设备。

-- +goose Down
DROP TABLE IF EXISTS tg_binding_tokens;

CREATE TABLE tg_binding_codes (
    code        TEXT        PRIMARY KEY,
    tg_user_id  BIGINT      NOT NULL,
    tg_username TEXT,
    tg_name     TEXT,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tg_binding_codes_live ON tg_binding_codes(tg_user_id) WHERE consumed_at IS NULL;
