-- +goose Up
-- 解析缓存从「每个资源一行」改成「每个资源的每个版本一行」。
--
-- 起因：缓存键此前等同于「上游的资源 ID」，而那两者只在 X/Twitter 这种
-- **编辑即换 ID** 的平台上重合。绝大多数 provider 是「ID 稳定、内容可变」——
-- YouTube 随时能改标题、Reddit 正文可编辑、Gist 不带 SHA 的 URL 永远指向最新
-- revision、Telegram 消息原地编辑且 message_id 不变。在这些平台上，旧的键
-- 与内容并非一一对应，于是缓存会在内容变更后**永久**返回旧结果。
--
-- 解法是让键指向「那条资源的那个版本」而不是「那条资源」：version 由上游的
-- 版本标记填充（TG 的 edit_date、其他 provider 的 revision/内容哈希）。
-- 同一 version 的重复解析天然幂等，不同 version 各占一行，查询取最新那行。
--
-- ============ 为什么 version 是独立列，而不是拼进 provider_resource_id ============
-- 把版本拼成 'chat123:456:<edit_date>' 看起来更省事（不动表结构），但查询就得写成
-- `provider_resource_id LIKE 'chat123:456:%'`，而在**非 C collation** 下 Postgres
-- 不会为 LIKE 前缀匹配使用普通 btree 索引。本库实测（50000 行，en_US.utf8）：
--
--   EXPLAIN SELECT ... WHERE provider_resource_id LIKE 'chat100:1000:%'
--   →  Seq Scan on parse_cache
--
-- 这张表的行数正是 000019 要解决的问题本身，让最热的读路径退化成顺序扫描是不可接受的。
-- 要救只能再建一个 text_pattern_ops 索引，那是为一个本可避免的编码方式付两份写放大。
-- 拆成列之后查询是纯等值匹配 + 索引序，下面的唯一约束就够用了。
ALTER TABLE parse_cache ADD COLUMN version TEXT NOT NULL DEFAULT '';

-- 空串 = 「该 provider 不提供版本信息」，语义上就是旧行为：唯一约束退化成
-- (provider, provider_resource_id)，每个资源恒一行。内置的 YouTube/Gist/Reddit/
-- GenericOG 全部落在这一档，无需任何特殊分支 —— 它们原样继续工作。
--
-- 加完立刻 DROP DEFAULT。留着默认值的话，将来接入的 provider 明明拿得到版本标记却
-- 忘了传，会静默写成空串 —— 表现是「多个版本挤进同一行、互相覆盖」，而这正是本次
-- 迁移要消灭的 bug，且它不会报错，只会让缓存悄悄退回旧行为。让它编译期/运行期直接
-- 失败，比让它安静地做错事好。（同 000016 给 platform 列的处理。）
ALTER TABLE parse_cache ALTER COLUMN version DROP DEFAULT;

COMMENT ON COLUMN parse_cache.version IS
    '上游的内容版本标记（TG edit_date / revision / 内容哈希）；空串表示该 provider 无版本信息，此时每资源恒一行。';

ALTER TABLE parse_cache
    DROP CONSTRAINT parse_cache_provider_provider_resource_id_key,
    ADD  CONSTRAINT parse_cache_provider_resource_version_key
         UNIQUE (provider, provider_resource_id, version);

-- **刻意不为「取最新版本」另建索引。** 上面的唯一约束已经提供
-- btree (provider, provider_resource_id, version)，等值前缀能直接定位到某个资源的
-- 全部版本行；而版本数有硬上界（parser.VersionsPerResource，应用侧每轮清理保证），
-- 在个位数行上再排一次序是零成本的。给全库最热的表之一多挂一个索引，写放大是每次
-- 解析都要付的，收益却只是省掉一次十行以内的排序 —— 这笔账不划算。

-- +goose Down
-- 恢复旧唯一约束之前必须先折叠版本行，否则同一资源的多个版本会让约束建不回来，
-- down 直接失败（而 down 的唯一职责是「让旧代码能跑起来」，失败的 down 毫无价值）。
--
-- 这一步**会删数据**，与 000019 的 down 刻意不回滚 expires_at 是同一类判断：
-- parse_cache 是重抓一次就能重建的缓存，不是业务数据。保留最新一版、丢弃历史版本，
-- 正好还原成旧代码所认识的形状。
DELETE FROM parse_cache WHERE id IN (
    SELECT id FROM (
        SELECT id, row_number() OVER (
            PARTITION BY provider, provider_resource_id ORDER BY created_at DESC, id DESC
        ) AS rn
        FROM parse_cache
    ) ranked
    WHERE rn > 1
);

ALTER TABLE parse_cache
    DROP CONSTRAINT IF EXISTS parse_cache_provider_resource_version_key,
    ADD  CONSTRAINT parse_cache_provider_provider_resource_id_key
         UNIQUE (provider, provider_resource_id);

ALTER TABLE parse_cache DROP COLUMN IF EXISTS version;
