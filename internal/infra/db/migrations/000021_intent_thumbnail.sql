-- +goose Up
-- 客户端生成的缩略图（成本审计 #7）。
--
-- 背景：ADR-069 把字节挪出 api 之后，服务端缩略图生成（ADR-050）对直传数据恒为
-- NULL —— api 手里没有字节，拉回来生成正好抵消掉直传省下的带宽。结果是列表里每一
-- 张图都在下载全尺寸原图：一张 4 MB 的照片本该只读 30 KB。改由客户端在混淆**之前**
-- 生成，随本体一起直传边缘，api 依然一个字节都不碰。
--
-- 两列挂在 upload_intents 而不是 files：它们描述的是「这次直传还答应传一张缩略图」，
-- 是签发时的承诺，收尾时兑现。兑现结果写进既有的 files.thumbnail_key，所以 files
-- 表一列都不用加 —— 下载票据、file GC、降级生命周期全都不需要知道这件事发生过。
--
-- 缩略图**不进 files 表、没有自己的 plain_hash**：它不是可被引用的实体，只是某个
-- file 的附属品。对象 key 仍由本体的 cipher_hash 推导（ThumbnailKeyFor），保持
-- files 行与缩略图对象严格 1:1 —— 全局去重（ADR-004）让多个用户共享同一行 files，
-- 而一行只有一个 thumbnail_key 槽位，缩略图若自带内容寻址就会出现两份抢一个槽。
--
-- CHECK 让「半张声明」不可表示。只填哈希不填大小的意图，收尾时无从校验 R2 里实际
-- 传上来多少字节 —— 而那是服务端唯一一次能确认客户端没有超量写入的机会。
ALTER TABLE upload_intents
    ADD COLUMN thumb_cipher_hash BYTEA,
    ADD COLUMN thumb_size_bytes  BIGINT,
    ADD CONSTRAINT chk_upload_intents_thumb
        CHECK ((thumb_cipher_hash IS NULL) = (thumb_size_bytes IS NULL));

COMMENT ON COLUMN upload_intents.thumb_size_bytes IS
    '混淆后的缩略图字节数，上限 file.MaxThumbBytes 且必须小于 size_bytes；NULL = 本次不带缩略图。';

-- +goose Down
ALTER TABLE upload_intents
    DROP CONSTRAINT IF EXISTS chk_upload_intents_thumb,
    DROP COLUMN IF EXISTS thumb_size_bytes,
    DROP COLUMN IF EXISTS thumb_cipher_hash;
