-- +goose Up
-- ADR-075 ⑩：三方账号绑定从「只有 Telegram」泛化为「按平台」。
--
-- 现在做而不是等到接微信时再做，理由只有一个：**表里还没有数据**。
-- 这里真正贵的不是改名，是 tg_user_id 的**类型**——Telegram 的用户 ID 是 int64，
-- 而微信 openid 是 28 字符的字符串，其它平台各有各的形状。一旦有了生产数据，
-- BIGINT → TEXT 是整表重写外加每个索引重建；今天它是一条 ALTER。
-- 换句话说：只改名不改类型等于没做这次迁移。
--
-- 唯一约束也跟着从全局挪到 (platform, platform_user_id)：留着全局唯一的话，
-- 一个恰好等于某个 Telegram 数字 ID 的微信 openid 会被拒绝，
-- 而那两个账号之间没有任何关系。

ALTER TABLE tg_bindings RENAME TO platform_bindings;

ALTER TABLE platform_bindings RENAME COLUMN tg_user_id  TO platform_user_id;
ALTER TABLE platform_bindings RENAME COLUMN tg_username TO platform_username;
ALTER TABLE platform_bindings RENAME COLUMN tg_name     TO platform_name;

-- 先带默认值加列以兼容存量行，再去掉默认值：留着 DEFAULT 'telegram' 的话，
-- 接入第二个平台时忘记传 platform 不会报错，只会静默写成 telegram —— 
-- 那种 bug 要等到用户发现「我绑的微信显示成 Telegram」才暴露。
ALTER TABLE platform_bindings ADD COLUMN platform TEXT NOT NULL DEFAULT 'telegram';
ALTER TABLE platform_bindings ALTER COLUMN platform DROP DEFAULT;

ALTER TABLE platform_bindings
    ALTER COLUMN platform_user_id TYPE TEXT USING platform_user_id::text;

-- 旧的列级 UNIQUE 是建表时写在 tg_user_id 上的，改名后约束仍在但语义已错。
ALTER TABLE platform_bindings DROP CONSTRAINT IF EXISTS tg_bindings_tg_user_id_key;
ALTER TABLE platform_bindings
    ADD CONSTRAINT uq_platform_bindings_account UNIQUE (platform, platform_user_id);

ALTER INDEX IF EXISTS idx_tg_bindings_user   RENAME TO idx_platform_bindings_user;
ALTER INDEX IF EXISTS idx_tg_bindings_device RENAME TO idx_platform_bindings_device;

-- RENAME TABLE 不会改主键约束的名字，留着 tg_bindings_pkey 的话，
-- 一次主键冲突报出来的表名是一张已经不存在的表。
ALTER TABLE platform_bindings RENAME CONSTRAINT tg_bindings_pkey TO platform_bindings_pkey;

-- 绑定令牌同样泛化。令牌本身命名的是一个 Squyrrl 账户、与平台无关，
-- 但**签发时就钉死平台**是一次免费的收紧：用户在 App 里点的是「绑定 Telegram」，
-- 那么这枚令牌就不该能在别处核销。没有这一列的话，将来两个平台共用一张令牌表，
-- 核销方是谁完全取决于用户把链接贴到哪里。
ALTER TABLE tg_binding_tokens RENAME TO binding_tokens;
ALTER TABLE binding_tokens ADD COLUMN platform TEXT NOT NULL DEFAULT 'telegram';
ALTER TABLE binding_tokens ALTER COLUMN platform DROP DEFAULT;

ALTER INDEX IF EXISTS idx_tg_binding_tokens_live RENAME TO idx_binding_tokens_live;
ALTER TABLE binding_tokens RENAME CONSTRAINT tg_binding_tokens_pkey TO binding_tokens_pkey;

-- +goose Down
ALTER INDEX IF EXISTS idx_binding_tokens_live RENAME TO idx_tg_binding_tokens_live;
ALTER TABLE binding_tokens RENAME CONSTRAINT binding_tokens_pkey TO tg_binding_tokens_pkey;
ALTER TABLE binding_tokens DROP COLUMN platform;
ALTER TABLE binding_tokens RENAME TO tg_binding_tokens;

ALTER INDEX IF EXISTS idx_platform_bindings_device RENAME TO idx_tg_bindings_device;
ALTER INDEX IF EXISTS idx_platform_bindings_user   RENAME TO idx_tg_bindings_user;
ALTER TABLE platform_bindings RENAME CONSTRAINT platform_bindings_pkey TO tg_bindings_pkey;

ALTER TABLE platform_bindings DROP CONSTRAINT IF EXISTS uq_platform_bindings_account;
-- 回滚会丢掉非 telegram 平台的行：它们的 platform_user_id 未必能转成 BIGINT，
-- 留着会让 ALTER TYPE 直接失败。宁可显式删，也好过迁移在半路报错。
DELETE FROM platform_bindings WHERE platform <> 'telegram';
ALTER TABLE platform_bindings
    ALTER COLUMN platform_user_id TYPE BIGINT USING platform_user_id::bigint;
ALTER TABLE platform_bindings DROP COLUMN platform;

ALTER TABLE platform_bindings RENAME COLUMN platform_name     TO tg_name;
ALTER TABLE platform_bindings RENAME COLUMN platform_username TO tg_username;
ALTER TABLE platform_bindings RENAME COLUMN platform_user_id  TO tg_user_id;
ALTER TABLE platform_bindings RENAME TO tg_bindings;
ALTER TABLE tg_bindings ADD CONSTRAINT tg_bindings_tg_user_id_key UNIQUE (tg_user_id);
