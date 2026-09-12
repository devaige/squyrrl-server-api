-- +goose Up
-- 给两张只增不减的表加上保留期。
--
-- 它们的共同点是**都不是业务数据**：parse_cache 是可以重新抓一次就有的缓存，
-- api_samples 的 live 行是排障用的采样。而托管 Postgres 按容量计费，
-- 这两张表的增长都直接挂在用量上 —— 越受欢迎，账单越高，且没有任何上界。

-- ============ 1. parse_cache：软过期从「默认永久」改成「默认有期限」 ============
-- 原先 Cache.Put 恒写 expires_at = NULL，注释叫它「软过期」，但实际上
-- 没有任何一行会过期。而 GenericOG 是兜底 provider，意味着**任意** http(s)
-- 链接都会落一行 —— 这张表的上界是「整个互联网」，不是「我们支持的那几个站点」。
--
-- 存量行按 created_at 回填，不按 now()：按 now() 回填等于给一批可能几个月前
-- 就没人再碰过的缓存重新续一个完整周期，恰好把「清掉最陈旧的那批」推到最后才发生。
-- 已经比保留期更老的行会拿到一个过去的时间戳，下一轮清理循环直接收掉，这是对的。
UPDATE parse_cache
SET expires_at = created_at + INTERVAL '30 days'
WHERE expires_at IS NULL;

-- 清理循环只扫有期限的行。条件写进部分索引而不是靠全表扫：
-- 这张表的行数正是问题本身，而按 expires_at 排序取一批是每轮都要做的事。
CREATE INDEX idx_parse_cache_expires
    ON parse_cache(expires_at)
    WHERE expires_at IS NOT NULL;

-- ============ 2. api_samples：live 采样限量 ============
-- kind='live' 在 endpoint.config.archive_live=true 时**每次调用都写一行**，
-- 成功与失败两条路径都写，内容是完整的请求 + 响应 JSON。
-- 它的用途（见 000010 的注释）是核对 response_map 有没有命中字段 ——
-- 那只需要最近几条，不是一份调用日志。
--
-- 限量用「每个 endpoint 保留最近 N 条」而不是「保留最近 N 天」：
-- 按时间保留在低频时留不下任何可看的样本，在高频时又留下几十万行；
-- 按条数保留两头都对，且上界与流量完全无关。
--
-- kind='sample'（管理员手工录入的契约示例）是业务数据，永不参与清理。
-- 现有索引 idx_api_samples_endpoint_created (endpoint_id, created_at DESC)
-- 正好是窗口函数要的顺序，不需要再加索引。
--
-- 这里**不删存量行**：种子 endpoint 目前 enabled=FALSE，表大概率是空的，
-- 而「迁移顺手删数据」是一种不该养成的习惯 —— 清理交给应用侧那个每轮都会跑的
-- 保留循环，第一轮跑完结果完全一样，区别只是它可回滚、可观察、出错不会卡住启动
-- （迁移在 main 里是 goose.Up，失败即进程退出）。

-- +goose Down
DROP INDEX IF EXISTS idx_parse_cache_expires;

-- expires_at 的回填**不回滚**。把它改回 NULL 会把一批本该过期的缓存变成永久行，
-- 也就是让 down 制造出比 up 之前更糟的状态。而 down 的目的只是「让旧代码能正常跑」，
-- 旧代码读 expires_at 时本来就认得非 NULL 值 —— Cache.Get 一直带着
-- `expires_at IS NULL OR expires_at > now()` 这个条件，从第一版就是。
