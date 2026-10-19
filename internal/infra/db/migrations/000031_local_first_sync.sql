-- +goose Up
-- ADR-084：本地优先 —— 创建与同步是两件独立的事（2026-10-19 用户决策）。
--
-- 三个时钟各管一件事，绝不混用：
--   created_at / updated_at —— 用户本机的墙上时间，只用来显示与排序，原样存储；
--   version                 —— 每条碎片的正文版本，判定内容冲突；
--   sync_seq                —— 每个账号一个递增计数器，增量拉取的游标。
-- 客户端时钟可以是错的（一台设备调到 2030 年），所以墙上时间永远不参与「谁先谁后」。

-- ① 每个账号一个计数器
CREATE TABLE sync_cursors (
    user_id        UUID   PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    seq            BIGINT NOT NULL DEFAULT 0,
    -- 服务端不留墓碑地删掉了**活着的**数据（降级后清理受限碎片）时 +1。
    -- 那类删除只针对云端副本，绝不能让设备删本机数据；客户端见到新值就对一次账，
    -- 把「本机以为已同步、服务端却没有」的碎片重新标成未同步。
    reconcile_gen  BIGINT NOT NULL DEFAULT 0
);

-- 取下一个号。计数器行在写入事务里被 upsert，行锁一直持有到事务提交 ——
-- 同一账号的写入因此按号的顺序提交，拉取永远不会先看见 11、事后才冒出 10。
-- 单个账号几乎没有并发写入，这把锁不构成成本。
-- +goose StatementBegin
CREATE FUNCTION next_sync_seq(uid UUID) RETURNS BIGINT AS $$
    INSERT INTO sync_cursors (user_id, seq) VALUES (uid, 1)
    ON CONFLICT (user_id) DO UPDATE SET seq = sync_cursors.seq + 1
    RETURNING seq;
$$ LANGUAGE sql;
-- +goose StatementEnd

-- 盖号放在触发器里：任何改动都必须换号，触发器是唯一不会被哪段代码忘掉的地方。
-- +goose StatementBegin
CREATE FUNCTION stamp_sync_seq() RETURNS TRIGGER AS $$
BEGIN
    NEW.sync_seq := next_sync_seq(NEW.user_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

ALTER TABLE snippets ADD COLUMN sync_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pages    ADD COLUMN sync_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tags     ADD COLUMN sync_seq BIGINT NOT NULL DEFAULT 0;

-- ② 标签改为软删除，并允许同名：身份只认 UUID，名字只用来显示（用户决策 B）。
ALTER TABLE tags ADD COLUMN deleted_at TIMESTAMPTZ;
ALTER TABLE tags DROP CONSTRAINT IF EXISTS tags_user_id_name_key;
CREATE INDEX idx_tags_user_active ON tags(user_id) WHERE deleted_at IS NULL;

-- ③ snippets.updated_at 不再由触发器维护：它是用户最后一次改正文的本机时间，
-- 由应用代码按客户端给的值写入。移动页面、改标签、受限标记都不该动它。
DROP TRIGGER IF EXISTS trg_snippets_updated_at ON snippets;

-- ④ 给存量数据补号：每个账号三张表统一编号。补号期间关掉页面/标签的
-- updated_at 触发器，否则这次 UPDATE 会把它们的时间全部改成迁移那一刻。
ALTER TABLE pages DISABLE TRIGGER trg_pages_updated_at;
ALTER TABLE tags  DISABLE TRIGGER trg_tags_updated_at;

CREATE TEMP TABLE sync_backfill ON COMMIT DROP AS
SELECT kind, id, user_id,
       row_number() OVER (PARTITION BY user_id ORDER BY updated_at, kind, id) AS seq
FROM (
    SELECT 'page' AS kind, id, user_id, updated_at FROM pages
    UNION ALL
    SELECT 'tag', id, user_id, updated_at FROM tags
    UNION ALL
    SELECT 'snippet', id, user_id, updated_at FROM snippets
) all_rows;

UPDATE pages p    SET sync_seq = b.seq FROM sync_backfill b WHERE b.kind = 'page'    AND b.id = p.id;
UPDATE tags t     SET sync_seq = b.seq FROM sync_backfill b WHERE b.kind = 'tag'     AND b.id = t.id;
UPDATE snippets s SET sync_seq = b.seq FROM sync_backfill b WHERE b.kind = 'snippet' AND b.id = s.id;
INSERT INTO sync_cursors (user_id, seq)
SELECT user_id, max(seq) FROM sync_backfill GROUP BY user_id;

ALTER TABLE pages ENABLE TRIGGER trg_pages_updated_at;
ALTER TABLE tags  ENABLE TRIGGER trg_tags_updated_at;

CREATE TRIGGER trg_snippets_sync_seq
    BEFORE INSERT OR UPDATE ON snippets
    FOR EACH ROW EXECUTE FUNCTION stamp_sync_seq();
CREATE TRIGGER trg_pages_sync_seq
    BEFORE INSERT OR UPDATE ON pages
    FOR EACH ROW EXECUTE FUNCTION stamp_sync_seq();
CREATE TRIGGER trg_tags_sync_seq
    BEFORE INSERT OR UPDATE ON tags
    FOR EACH ROW EXECUTE FUNCTION stamp_sync_seq();

CREATE INDEX idx_snippets_user_seq ON snippets(user_id, sync_seq);
CREATE INDEX idx_pages_user_seq    ON pages(user_id, sync_seq);
CREATE INDEX idx_tags_user_seq     ON tags(user_id, sync_seq);

-- ⑤ 改变「拉取一条碎片会拿到什么」的东西都必须给那条碎片换号。
-- 碎片自己的 UPDATE 已由 ③ 之后的触发器覆盖；这里补上另外两处：
-- 标签关联（增删标签、删标签时摘掉关联）与附件文件的缩略图回填。
-- 「SET sync_seq = 0」只是为了触发一次 UPDATE，真正的号由 stamp_sync_seq 填。
--
-- 标签关联用语句级触发器 + 转换表：一次替换整组标签只给碎片换一次号，而不是每行一次。
-- +goose StatementBegin
CREATE FUNCTION touch_snippets_from_tag_links() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        UPDATE snippets SET sync_seq = 0
        WHERE id IN (SELECT DISTINCT snippet_id FROM old_links);
    ELSE
        UPDATE snippets SET sync_seq = 0
        WHERE id IN (SELECT DISTINCT snippet_id FROM new_links);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_snippet_tags_touch_insert
    AFTER INSERT ON snippet_tags
    REFERENCING NEW TABLE AS new_links
    FOR EACH STATEMENT EXECUTE FUNCTION touch_snippets_from_tag_links();
CREATE TRIGGER trg_snippet_tags_touch_delete
    AFTER DELETE ON snippet_tags
    REFERENCING OLD TABLE AS old_links
    FOR EACH STATEMENT EXECUTE FUNCTION touch_snippets_from_tag_links();

-- 缩略图是上传收尾之后异步挂上的：不换号的话，在那之前拉到这条碎片的设备
-- 会永远以为它没有缩略图。
-- +goose StatementBegin
CREATE FUNCTION touch_snippets_of_file() RETURNS TRIGGER AS $$
BEGIN
    UPDATE snippets SET sync_seq = 0
    WHERE id IN (SELECT snippet_id FROM elements WHERE file_id = NEW.id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_files_touch_snippets
    AFTER UPDATE OF thumbnail_key, thumb_width, thumb_height ON files
    FOR EACH ROW EXECUTE FUNCTION touch_snippets_of_file();

-- ⑥ 回收站到期硬删时留下的墓碑：只有 id 与号，永久保留。
-- 离线超过回收站保留期的设备回来时，靠它才知道「这条是用户删的」。
-- 降级后清理受限碎片**不**写墓碑（见 sync_cursors.reconcile_gen）。
CREATE TABLE snippet_tombstones (
    id        UUID   PRIMARY KEY,
    user_id   UUID   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sync_seq  BIGINT NOT NULL
);
CREATE INDEX idx_snippet_tombstones_user_seq ON snippet_tombstones(user_id, sync_seq);

-- ⑦ 登录后未归属数据自动归属到账户（用户决策 A），批量迁移接口随之删除。
DROP TABLE IF EXISTS anonymous_claims;


-- +goose Down
-- 标签名 UNIQUE 恢复时若已有同名标签会失败 —— 那正是本迁移允许出现的数据，
-- 回滚前需先人工处理。
CREATE TABLE anonymous_claims (
    id              UUID        PRIMARY KEY,
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    snippet_count   INTEGER     NOT NULL DEFAULT 0,
    page_count      INTEGER     NOT NULL DEFAULT 0,
    tag_count       INTEGER     NOT NULL DEFAULT 0,
    element_count   INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_anonymous_claims_user ON anonymous_claims(user_id, created_at DESC);

DROP TABLE IF EXISTS snippet_tombstones;
DROP TRIGGER IF EXISTS trg_files_touch_snippets ON files;
DROP FUNCTION IF EXISTS touch_snippets_of_file();
DROP TRIGGER IF EXISTS trg_snippet_tags_touch_delete ON snippet_tags;
DROP TRIGGER IF EXISTS trg_snippet_tags_touch_insert ON snippet_tags;
DROP FUNCTION IF EXISTS touch_snippets_from_tag_links();

DROP INDEX IF EXISTS idx_tags_user_seq;
DROP INDEX IF EXISTS idx_pages_user_seq;
DROP INDEX IF EXISTS idx_snippets_user_seq;
DROP TRIGGER IF EXISTS trg_tags_sync_seq ON tags;
DROP TRIGGER IF EXISTS trg_pages_sync_seq ON pages;
DROP TRIGGER IF EXISTS trg_snippets_sync_seq ON snippets;

CREATE TRIGGER trg_snippets_updated_at
    BEFORE UPDATE ON snippets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DELETE FROM tags WHERE deleted_at IS NOT NULL;
DROP INDEX IF EXISTS idx_tags_user_active;
ALTER TABLE tags ADD CONSTRAINT tags_user_id_name_key UNIQUE (user_id, name);
ALTER TABLE tags DROP COLUMN deleted_at;

ALTER TABLE tags     DROP COLUMN sync_seq;
ALTER TABLE pages    DROP COLUMN sync_seq;
ALTER TABLE snippets DROP COLUMN sync_seq;

DROP FUNCTION IF EXISTS stamp_sync_seq();
DROP FUNCTION IF EXISTS next_sync_seq(UUID);
DROP TABLE IF EXISTS sync_cursors;
