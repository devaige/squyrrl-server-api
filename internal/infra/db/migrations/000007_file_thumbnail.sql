-- +goose Up
-- 服务端预生成图片缩略图：files 表加 thumbnail_key 列。
-- 非图片 mime 或生成失败的文件该列保持 NULL，客户端就只看到「无缩略图」状态。
-- 对象 key 约定：`thumb/<storage_key>.jpg`（同一 bucket，JPEG 输出避免 CGO）；详见 ADR-050。
ALTER TABLE files
    ADD COLUMN thumbnail_key TEXT;


-- +goose Down
ALTER TABLE files
    DROP COLUMN IF EXISTS thumbnail_key;
