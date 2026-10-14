-- +goose Up
-- 文件与缩略图的像素尺寸（2026-10-14 用户决策：列表卡片按附件预览图排版）。
--
-- 卡片要在拿到任何字节之前就定下版式：单图带文本时，预览图比卡片内容区宽就走大图，
-- 否则走小图。预览图是缩略图（有的话）或原图本身（小图片不生成缩略图），所以两组
-- 尺寸都要存。字节进不了 api（ADR-069），尺寸只能由上传方声明 —— 客户端解码时、
-- Bot 从 TG 的 PhotoSize 里都拿得到。它们只影响排版，声明错了的后果是版式不对，
-- 不是安全问题，所以服务端只做范围校验，不去核实。
--
-- 挂在 files 而不是 elements：尺寸是内容的固有属性，全局去重（ADR-004）下同一行
-- files 被多个用户共享，存在 elements 上就是同一个事实的 N 份副本。
-- 缩略图尺寸与 thumbnail_key 同生共死，但不强制「有 key 必有尺寸」：历史缩略图
-- 没有尺寸，客户端把未知尺寸当小图处理。
ALTER TABLE files
    ADD COLUMN width        INTEGER,
    ADD COLUMN height       INTEGER,
    ADD COLUMN thumb_width  INTEGER,
    ADD COLUMN thumb_height INTEGER,
    ADD CONSTRAINT chk_files_dims
        CHECK ((width IS NULL AND height IS NULL) OR (width > 0 AND height > 0)),
    ADD CONSTRAINT chk_files_thumb_dims
        CHECK ((thumb_width IS NULL AND thumb_height IS NULL) OR (thumb_width > 0 AND thumb_height > 0));

-- 直传的尺寸在签发时声明、收尾时落进 files，与缩略图声明（000021）同一个节奏。
ALTER TABLE upload_intents
    ADD COLUMN width        INTEGER,
    ADD COLUMN height       INTEGER,
    ADD COLUMN thumb_width  INTEGER,
    ADD COLUMN thumb_height INTEGER,
    ADD CONSTRAINT chk_upload_intents_dims
        CHECK ((width IS NULL AND height IS NULL) OR (width > 0 AND height > 0)),
    ADD CONSTRAINT chk_upload_intents_thumb_dims
        CHECK ((thumb_width IS NULL AND thumb_height IS NULL) OR (thumb_width > 0 AND thumb_height > 0));

-- +goose Down
ALTER TABLE upload_intents
    DROP CONSTRAINT IF EXISTS chk_upload_intents_thumb_dims,
    DROP CONSTRAINT IF EXISTS chk_upload_intents_dims,
    DROP COLUMN IF EXISTS thumb_height,
    DROP COLUMN IF EXISTS thumb_width,
    DROP COLUMN IF EXISTS height,
    DROP COLUMN IF EXISTS width;

ALTER TABLE files
    DROP CONSTRAINT IF EXISTS chk_files_thumb_dims,
    DROP CONSTRAINT IF EXISTS chk_files_dims,
    DROP COLUMN IF EXISTS thumb_height,
    DROP COLUMN IF EXISTS thumb_width,
    DROP COLUMN IF EXISTS height,
    DROP COLUMN IF EXISTS width;
