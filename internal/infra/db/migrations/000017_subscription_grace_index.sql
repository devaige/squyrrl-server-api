-- +goose Up
-- ADR-075 ⑭ 的配套索引。
--
-- 宽限期落地后，「用户现在算哪一档」的判据从 `status = 'active'` 变成
-- `status = 'active' OR (status = 'past_due' AND …)`。既有的
-- idx_subscriptions_user_active 是 `WHERE status = 'active'` 的部分索引，
-- 覆盖不到 OR 的另一半，于是这条查询会退化成全表扫描 ——
-- 而它在**每次配额检查**上都要跑一遍（新建碎片、新建页面、绑定三方号…）。
--
-- 新索引的谓词是旧的超集，所以 SupersedeActivePlan 那类纯 active 的查询同样能用它
-- （查询谓词蕴含索引谓词时 Postgres 可以走部分索引），旧索引因此可以直接下掉，
-- 不必为同一件事付两份写入代价。
CREATE INDEX idx_subscriptions_user_kind_live
    ON subscriptions(user_id, kind)
    WHERE status IN ('active', 'past_due');

DROP INDEX IF EXISTS idx_subscriptions_user_active;

-- ADR-075 ⑯ 的配套索引：回收站清理每轮先做一次
-- `WHERE deleted_at IS NOT NULL AND deleted_at < …` 的粗筛。
-- 没有索引的话，那是对 snippets（本库最大的表）的全表扫描，每 6 小时一次。
-- 部分索引只收录软删的行 —— 而软删的行本来就是少数，索引因此很小。
CREATE INDEX idx_snippets_trash
    ON snippets(deleted_at)
    WHERE deleted_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_snippets_trash;

CREATE INDEX idx_subscriptions_user_active
    ON subscriptions(user_id, kind)
    WHERE status = 'active';

DROP INDEX IF EXISTS idx_subscriptions_user_kind_live;
