package quota

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/features/entitlement"
	"github.com/squyrrl/api/internal/features/pricing"
)

// PlanReader 提供用户当前档位。由 wallet.Service 实现 —— 用接口而不是直接依赖，
// 是为了让 quota 的单测不必拖进一个数据库连接池。
type PlanReader interface {
	ActivePlan(ctx context.Context, userID uuid.UUID) (string, error)
}

type Service struct {
	pool  *pgxpool.Pool
	plans PlanReader
}

func NewService(pool *pgxpool.Pool, plans PlanReader) *Service {
	return &Service{pool: pool, plans: plans}
}

// tierOf 取用户档位对应的能力表。
//
// 未知档位一律按 free 处理（MustOf 的语义）：档位字符串来自 subscriptions 表，
// 若某天出现一个代码不认识的值，宁可保守限制也不要放行 —— 放行的代价是成本泄漏，
// 而保守的代价只是一次用户投诉。
func (s *Service) tierOf(ctx context.Context, userID uuid.UUID) (entitlement.Tier, string, error) {
	plan, err := s.plans.ActivePlan(ctx, userID)
	if err != nil {
		return entitlement.Tier{}, "", err
	}
	return entitlement.MustOf(plan), plan, nil
}

// DeviceLimit 返回该用户档位允许的设备数。0 表示该档位不适用设备概念
// （免费档纯本地），调用方**不能**把它理解成「一台都不许有」。
func (s *Service) DeviceLimit(ctx context.Context, userID uuid.UUID) (int, error) {
	t, _, err := s.tierOf(ctx, userID)
	if err != nil {
		return 0, err
	}
	return t.Devices, nil
}

// PlanOf 暴露用户当前档位字符串，供需要在错误响应里回填 plan 的调用方使用。
func (s *Service) PlanOf(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.plans.ActivePlan(ctx, userID)
}

// RequireSync 断言用户所在档位允许把数据同步到服务端。
//
// 这是所有门槛里最重要的一道，因为它直接对应服务端成本：免费档是纯本地应用
// （ADR-075），它的碎片压根不该出现在 Postgres 里。客户端已经在本地分支处理，
// 服务端这一层是防御 —— 客户端被改造或有人直接打 API 时仍然拦得住。
func (s *Service) RequireSync(ctx context.Context, userID uuid.UUID) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}
	return nil
}

// CheckSnippetCreate 在新建碎片前校验总量。
func (s *Service) CheckSnippetCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitSnippets,
		`SELECT count(*) FROM snippets WHERE user_id = $1 AND deleted_at IS NULL`)
}

// CheckPageCreate 在新建页面前校验总量。系统页面（冲突收件箱）不计入 ——
// 它由服务端按需自建，把它算进用户配额会导致「用户什么都没做却少了一个额度」。
func (s *Service) CheckPageCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitPages,
		`SELECT count(*) FROM pages WHERE user_id = $1 AND deleted_at IS NULL AND is_system = FALSE`)
}

// CheckTagCreate 在新建标签前校验总量。
func (s *Service) CheckTagCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitTags,
		`SELECT count(*) FROM tags WHERE user_id = $1`)
}

// deviceCountSQL 数「真正的设备」。
//
// 排除三方绑定占位的那些：Telegram 绑定会建一台 platform=telegram 的设备
// （tg.CreateBindingDevice），但它已经被 BindingsPerPlatform 这条门槛管着了。
// 两条门槛同时计一样东西，用户会看到「我只登了一台电脑，怎么说我有两台设备」——
// 而且绑一个 TG 号会白白吃掉一个登录名额，那不是档位表的意思：
// 设备数与每平台绑定数在 ADR-075 里是两个独立的允许量。
const deviceCountSQL = `
	SELECT count(*) FROM devices d
	WHERE d.user_id = $1 AND d.revoked_at IS NULL
	  AND NOT EXISTS (SELECT 1 FROM platform_bindings pb WHERE pb.device_id = d.id)`

// CheckDeviceCreate 在注册新设备前校验数量。
//
// **尚未接入任何调用点**：既有设计是「超限时踢掉最久未活跃的那台」而不是拒绝新登录
// （见 docs/context.md 的 Device limit 一条），那是一条独立的逐出链路，
// 而它落在登录路径上 —— 做错的后果是把人挡在门外。留待单独一批。
func (s *Service) CheckDeviceCreate(ctx context.Context, userID uuid.UUID) error {
	return s.checkCount(ctx, userID, LimitDevices, deviceCountSQL)
}

// CheckBindingCreate 在绑定新的三方账号前校验数量。
//
// 计数带 platform 条件：BindingsPerPlatform 的字面含义是「每个平台各多少个」，
// 不是所有平台合计。合计口径会让接入微信之后，已经绑满 Telegram 的用户
// 一个微信号都绑不了 —— 而他并没有多占任何成本。
func (s *Service) CheckBindingCreate(ctx context.Context, userID uuid.UUID, platform string) error {
	return s.checkCount(ctx, userID, LimitBindings,
		`SELECT count(*) FROM platform_bindings WHERE user_id = $1 AND platform = $2`,
		platform)
}

// CheckHiddenPage 校验「隐藏页面」这项能力。它是布尔门槛，没有数量概念。
func (s *Service) CheckHiddenPage(ctx context.Context, userID uuid.UUID) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.HiddenPages {
		return newCapabilityError(LimitHiddenPages, plan, firstTierWith(func(x entitlement.Tier) bool {
			return x.HiddenPages
		}))
	}
	return nil
}

// checkCount 是数量型门槛的共同实现：取档位上限，count 当前用量，超了就拒。
//
// 每次 create 都多一次 count 查询是有代价的，但这些表都有 user_id 的部分索引，
// 而 create 本身也不是高频路径。真到了需要优化的那天，正确做法是维护计数器列
// 并定期对账，而不是取消检查 —— 但那属于过早优化，现在不做。
// countSQL 恒以 $1 = userID 开头，extra 依次填 $2 起 —— 目前只有按平台计数用到。
func (s *Service) checkCount(ctx context.Context, userID uuid.UUID, limit Limit, countSQL string, extra ...any) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}

	max := capOf(t, limit)
	args := append([]any{userID}, extra...)
	var current int
	if err := s.pool.QueryRow(ctx, countSQL, args...).Scan(&current); err != nil {
		return err
	}
	if current >= max {
		return newLimitError(limit, plan, max, current)
	}
	return nil
}

// firstTierWith 返回第一个满足条件的档位键，用于布尔能力的「需要升到哪一档」。
func firstTierWith(pred func(entitlement.Tier) bool) string {
	for _, key := range entitlement.Order {
		if t, ok := entitlement.Of(key); ok && pred(t) {
			return key
		}
	}
	return ""
}

// CheckUpload 在**传输开始之前**判定这个文件放不放得下。
//
// 三道判据，顺序是有讲究的：
//  1. 档位是否允许存储 —— 免费档的碎片只在本机，云端字节没有东西可挂（ADR-075）；
//  2. 单文件上限（由总配额派生，见 pricing.MaxFileBytesFor）；
//  3. 总量：已占用 + **在途预留** + 本次 ≤ 配额。
//
// 第 3 条里的「在途预留」是这个函数存在的真正理由。直传之后字节不经过 api
// （ADR-069），服务端唯一能施加约束的时刻就是签发意图那一下。只比对已落库的占用，
// 十个并发上传会各自看到同一个「还剩多少」，然后一起超卖 —— 而超出去的是
// 已经躺在 R2 里、要按月付费的字节。
func (s *Service) CheckUpload(ctx context.Context, userID uuid.UUID, size int64) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}

	st, err := s.Storage(ctx, userID)
	if err != nil {
		return err
	}
	quotaGB := int(st.QuotaBytes / (1024 * 1024 * 1024))

	maxFile := pricing.MaxFileBytesFor(quotaGB)
	if maxFile <= 0 {
		// 没买存储。这不是「档位不够」——任何档位单独都给不了存储，
		// 要的是另一件商品，所以 required_plan 留空、由文案说清该买什么。
		return newStorageError("当前账户没有云存储空间，购买后即可上传文件", plan, st, size)
	}
	if size > maxFile {
		return newStorageError(
			fmt.Sprintf("单个文件最大 %d GB，扩容后上限会随之提升", maxFile/(1024*1024*1024)),
			plan, st, size)
	}

	held, err := s.heldBytes(ctx, userID)
	if err != nil {
		return err
	}
	if st.UsedBytes+held+size > st.QuotaBytes {
		return newStorageError("云存储空间不足", plan, st, size)
	}
	return nil
}

// heldBytes 是这个用户手上尚未收尾的上传意图占掉的字节。
//
// 只算未过期的 pending：过期意图由 sweeper 回收（它同时会 abort R2 的分片），
// 把它们继续算进预留，等于让一次没传完的上传把额度锁到天荒地老。
func (s *Service) heldBytes(ctx context.Context, userID uuid.UUID) (int64, error) {
	var held int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0) FROM upload_intents
		WHERE user_id = $1 AND status = 'pending' AND expires_at > now()`,
		userID).Scan(&held)
	return held, err
}

// StorageStatus 是用户的云存储配额与占用，单位统一为字节。
type StorageStatus struct {
	QuotaBytes int64 `json:"quota_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
}

// Storage 汇总用户的存储配额与实际占用。
//
// 配额来自所有 active 的 storage 订阅之和 —— 它们可以叠加（ADR-075），
// 所以这里是 SUM 而不是取最新一条。plan **不贡献任何配额**：三块商品互相独立，
// 免费档和至尊版自带的存储都是 0。
//
// 占用按「每个引用者全额计」口径（ADR-075 用户决策）：
// DISTINCT 在用户内部去重（同一用户的多条碎片引用同一文件只算一次），
// 但跨用户各算各的 —— 全局去重（ADR-026）省下的字节留在成本侧，不回馈给用户。
// 反过来做的话，别人删掉一个共享文件会让你的占用凭空上涨，那是没人能理解的账单。
func (s *Service) Storage(ctx context.Context, userID uuid.UUID) (StorageStatus, error) {
	var st StorageStatus

	// past_due 同样吃宽限期，判据与 plan 一致（ADR-075 ⑭）。
	// 这一侧的后果比 plan 轻 —— 配额掉零只是传不了新文件，已传的字节不会消失 ——
	// 但两处用不同的判据会制造一种没人能解释的中间态：订阅还在服务，
	// 而同一次扣款失败已经让存储停摆。
	// 存储只提供年付（ADR-075），所以这里事实上恒取年付窗口；仍写成 CASE 是为了
	// 让「万一以后开了月付存储」不会静默套用一个更长的窗口。
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(bonus_storage_gb), 0)::bigint * 1024 * 1024 * 1024
		FROM subscriptions
		WHERE user_id = $1 AND kind = 'storage'
		  AND (status = 'active'
		       OR (status = 'past_due'
		           AND current_period_end
		               + (CASE billing_period WHEN 'yearly' THEN $2::interval ELSE $3::interval END) > now()))`,
		userID, pricing.GraceYearly.String(), pricing.GraceMonthly.String()).Scan(&st.QuotaBytes); err != nil {
		return st, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(f.size_bytes), 0)
		FROM (
			SELECT DISTINCT e.file_id
			FROM elements e
			JOIN snippets s ON s.id = e.snippet_id
			WHERE s.user_id = $1 AND s.deleted_at IS NULL AND e.file_id IS NOT NULL
		) uf
		JOIN files f ON f.id = uf.file_id`,
		userID).Scan(&st.UsedBytes); err != nil {
		return st, err
	}

	return st, nil
}

// Usage 是用户当前的资源占用，与 entitlement.Tier 的同名字段对应。
type Usage struct {
	Snippets int `json:"snippets"`
	Pages    int `json:"pages"`
	Tags     int `json:"tags"`
	Devices  int `json:"devices"`
}

// CurrentUsage 一次查回全部用量。
//
// 用单条多子查询而不是四次往返：客户端需要「已用 / 上限」来在本地预判
// （比如迁移界面里实时算「还能再选多少条」），这条路径会被频繁调用，
// 四次 round-trip 的延迟在移动网络上是能感知的。
func (s *Service) CurrentUsage(ctx context.Context, userID uuid.UUID) (Usage, error) {
	var u Usage
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM snippets WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM pages    WHERE user_id = $1 AND deleted_at IS NULL AND is_system = FALSE),
			(SELECT count(*) FROM tags     WHERE user_id = $1),
			(SELECT count(*) FROM devices d WHERE d.user_id = $1 AND d.revoked_at IS NULL
			   AND NOT EXISTS (SELECT 1 FROM platform_bindings pb WHERE pb.device_id = d.id))`,
		userID).Scan(&u.Snippets, &u.Pages, &u.Tags, &u.Devices)
	return u, err
}

// CheckBatch 校验一次批量写入（匿名数据迁移）是否会让任一资源越过档位上限。
//
// 这是**防御性**校验，不是用户体验的一部分：正常流程下客户端已经在迁移界面里
// 按实时余量约束了用户的选择，走到这里就该是通过的。它存在是因为服务端不能
// 信任客户端 —— 一个改造过的客户端可以直接提交超量数据。
//
// 三项分别报错而不是合并成一条：用户需要知道是碎片超了还是标签超了，
// 两者的处理方式完全不同（少选几条 vs 合并几个标签）。
func (s *Service) CheckBatch(ctx context.Context, userID uuid.UUID, addSnippets, addPages, addTags int) error {
	t, plan, err := s.tierOf(ctx, userID)
	if err != nil {
		return err
	}
	if !t.Sync {
		return ErrSyncRequired(plan)
	}

	u, err := s.CurrentUsage(ctx, userID)
	if err != nil {
		return err
	}

	for _, c := range []struct {
		limit   Limit
		current int
		add     int
		max     int
	}{
		{LimitSnippets, u.Snippets, addSnippets, t.Snippets},
		{LimitPages, u.Pages, addPages, t.Pages},
		{LimitTags, u.Tags, addTags, t.Tags},
	} {
		if c.add > 0 && c.current+c.add > c.max {
			// 报「加完之后会是多少」而不是当前值：用户要判断的是这一批能不能放下。
			return newLimitError(c.limit, plan, c.max, c.current+c.add)
		}
	}
	return nil
}
