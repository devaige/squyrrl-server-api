-- +goose Up
-- ADR-075：降级后超额数据的生命周期（2026-09-11 用户决策）。
--
--   正常 → 宽限 30 天（可读、可删，不可改）→ 冻结 30 天（列表可见，详情不可看）→ 永久删除
--
-- 只加一列而不是一个状态枚举：三个阶段全部由**同一个时间戳**推导，
-- 状态与时间因此不可能不一致。若改用枚举，就必须有一个定时任务去推进它，
-- 而那个任务哪怕停一天，用户看到的状态就是错的 —— 冻结期到了却还在宽限，
-- 或者反过来。推导出来的状态不会有这个问题。
--
-- restricted_at 是「这条碎片进入受限生命周期的时刻」：
--   NULL                              → 正常
--   now() < restricted_at + 30 天     → 宽限期
--   now() < restricted_at + 60 天     → 冻结期
--   否则                               → 等待清理
--
-- 置空即完全恢复（用户升档回来，或删掉足够多的数据使其重回额度内）。
-- 之后若再次超额，时钟从头开始 —— 对用户宽松的一侧，且省掉一个「累计已用宽限」的状态。
ALTER TABLE snippets ADD COLUMN restricted_at TIMESTAMPTZ;

-- 部分索引：受限的碎片在任何健康账户里都是零行，只有降级用户才有。
-- 清理任务与「列出受限条目」两条路径都靠它。
CREATE INDEX idx_snippets_restricted
    ON snippets(restricted_at)
    WHERE restricted_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_snippets_restricted;
ALTER TABLE snippets DROP COLUMN restricted_at;
