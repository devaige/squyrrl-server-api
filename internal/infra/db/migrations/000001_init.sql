-- +goose Up
-- 启用基础扩展。citext 用于邮箱大小写不敏感比较；pgcrypto 提供 gen_random_uuid 等。
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- +goose Down
DROP EXTENSION IF EXISTS pgcrypto;
DROP EXTENSION IF EXISTS citext;
