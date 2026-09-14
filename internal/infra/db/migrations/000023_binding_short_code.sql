-- +goose Up
-- 绑定令牌增加「短码 + 待确认」两段状态（用户决策 2026-09-14）。
--
-- 起因是 000012 没覆盖到的一个组合：App 在手机上、Telegram 在电脑上。那时给令牌
-- 定的两条搬运路径 —— 点 deep link（要求同一台设备装了 TG）与扫二维码（要求 TG
-- 那台设备有摄像头）—— 在这个组合下同时不成立，用户走进死路。
--
-- 补第三条路径就得重新给出一个人眼能读、手能敲的码，而 000012 砍掉手输码的理由
-- 恰恰是「一串人眼可读、会被顺手粘进群的码」。这次能加回来，是因为码不再是持票
-- 凭证：Bot 拿码只能把令牌标成 claimed 并登记「谁在申请」，真正建立绑定要账户
-- 主人在 App 里看着那个 @username 点一次确认。码泄漏到群里，捡到的人最多让机主
-- 弹出一个写着陌生 handle 的确认框 —— 这正是 OAuth Device Flow 敢把 user_code
-- 印在电视屏幕上的同一个理由。
--
-- deep link 与扫码**不走确认**，仍是 consume 一步到位。确认是针对「载体被人眼
-- 搬运过」这一点的补偿，而链接是用户几秒前在自己设备上生成、自己点掉的，
-- 给它加一步纯属摩擦。

ALTER TABLE binding_tokens ADD COLUMN short_code             TEXT;
ALTER TABLE binding_tokens ADD COLUMN claimed_at             TIMESTAMPTZ;
ALTER TABLE binding_tokens ADD COLUMN claim_platform_user_id TEXT;
ALTER TABLE binding_tokens ADD COLUMN claim_username         TEXT;
ALTER TABLE binding_tokens ADD COLUMN claim_name             TEXT;

-- 唯一性只在未核销的码之间成立。短码只有 31^8 ≈ 2^39.6 的空间，全表唯一会在
-- 若干年后开始碰撞并让签发失败；而真正需要保证的性质是「同一时刻不存在两枚可兑换
-- 的相同短码」。索引也因此只有活跃码那么大。既有行的 short_code 为 NULL，
-- 唯一索引不约束 NULL，回填无必要。
CREATE UNIQUE INDEX idx_binding_tokens_short_code
    ON binding_tokens(short_code) WHERE consumed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_binding_tokens_short_code;
ALTER TABLE binding_tokens DROP COLUMN IF EXISTS claim_name;
ALTER TABLE binding_tokens DROP COLUMN IF EXISTS claim_username;
ALTER TABLE binding_tokens DROP COLUMN IF EXISTS claim_platform_user_id;
ALTER TABLE binding_tokens DROP COLUMN IF EXISTS claimed_at;
ALTER TABLE binding_tokens DROP COLUMN IF EXISTS short_code;
