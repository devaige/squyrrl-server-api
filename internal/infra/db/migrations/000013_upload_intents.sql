-- +goose Up
-- 客户端直传 R2 的上传意图表（ADR-069）。
--
-- 存在的理由不是「防重复上传」——storage_key = hex(cipher_hash) 是内容寻址，
-- 两个客户端并发写同一个 key 的结果完全一致，重复只浪费一次带宽、不产生正确性问题。
-- 真正的理由是：直传之后字节不再经过 api，服务端必须留下「我签发过哪些 key、期望多大」
-- 的记录，否则 ① 客户端传完却没 commit 的对象在 R2 里成为孤儿，而现有 GC 是从
-- files 表反查的、根本扫不到；② 配额无法在传输前预留（见 docs/context.md 的 TG 大文件条目）。
--
-- plain_hash 刻意不加 UNIQUE：并发申请同一文件是允许的（见上，幂等），
-- 加唯一约束反而会让第二个申请者拿不到 token。
CREATE TABLE upload_intents (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plain_hash      BYTEA       NOT NULL,
    cipher_hash     BYTEA       NOT NULL,
    size_bytes      BIGINT      NOT NULL,                 -- 混淆后的字节数（= 实际传输量）
    mime            TEXT        NOT NULL DEFAULT 'application/octet-stream',
    storage_key     TEXT        NOT NULL,
    r2_upload_id    TEXT,                                 -- multipart 的 uploadId；单片模式为 NULL
    part_size       INT         NOT NULL,
    part_count      INT         NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending',   -- pending | committed | aborted
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL
);

-- sweeper 的唯一查询形状：按 (status, expires_at) 找过期 pending
CREATE INDEX idx_upload_intents_sweep ON upload_intents (status, expires_at);

-- +goose Down
DROP TABLE IF EXISTS upload_intents;
