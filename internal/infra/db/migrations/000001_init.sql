-- +goose Up
-- citext：邮箱大小写不敏感比较，用于 users.email 与 email_otps.email 两列。
--
-- **托管数据库上这句是空操作，扩展要由管理员预先建好。** 阿里云 RDS / 多数托管 PG 都不
-- 允许普通账号 CREATE EXTENSION，而迁移是用应用账号跑的，直接报
-- `permission denied to create extension "citext"` (SQLSTATE 42501)。
-- 已实测：管理员建好之后，普通账号跑本句会得到 `NOTICE: already exists, skipping`
-- 并正常返回 —— 所以这里保留 IF NOT EXISTS 而不是删掉它：
-- 本地开发是超级用户，这句让 `docker compose up` 零前置就能跑起来；
-- 生产则退化为无害的空操作。部署前置见 docs/readme/03-server-prod.md。
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto 已移除（2026-09-09）：schema 里唯一用到的是 gen_random_uuid()，
-- 而它自 PostgreSQL 13 起已是**内置函数**，不再需要 pgcrypto。已实测：只装 citext 的
-- PG 16 上 gen_random_uuid() 正常可用。少一个扩展就少一次托管库上要不到的特权操作。

-- +goose Down
DROP EXTENSION IF EXISTS citext;
