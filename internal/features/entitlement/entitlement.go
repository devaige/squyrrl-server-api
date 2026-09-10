// Package entitlement 定义基础订阅各档位的能力与容量上限。
//
// ADR-075 把增值服务拆成三样独立商品：基础订阅、credits、存储。
// 本包只管第一样 —— 它**不含任何 credits 赠送、不含任何存储配额**，
// 那两样各有自己的计费与核算路径，互不换算。
//
// 之所以做成一张表而不是散落各处的 `if tier == "premium"`：
// 门槛点有十来个、档位有五个，写成条件分支就是五十处需要同步修改的地方，
// 而漏改一处的表现是「某档悄悄多给了额度」，没有任何测试会红。
// 表的形态让「加一档」退化成加一行，让「改一个门槛」只有一个落点。
package entitlement

// Tier 是一个基础订阅档位的全部含义。
//
// 字段全是具体数值，没有「无限」哨兵：ADR-075 定的顶档是 1000 万碎片而非无限，
// 因为碎片会落进 Postgres（≈2 KB/条），是这张表里唯一真实随用量增长的成本项。
// 声称无限就等于把最坏情况写成了无限。
type Tier struct {
	Key string

	// PriceUSDMonthly 月付价（美元）。年付 = 该值 × 10（省两个月）。
	// 暂时硬编码：调价目前需要发版，后续由 GET /pricing 从 DB 覆盖（ADR-075 待办 ⑥）。
	PriceUSDMonthly int

	// Sync 决定碎片元数据是否同步到服务端。
	//
	// free 为 false —— 免费档是**纯本地应用**，服务端不存它的任何碎片，
	// 因此免费用户的服务端成本严格为零。客户端复用 ADR-051 的匿名分支实现，
	// 判据从 `tokens == null` 放宽为 `tokens == null || plan == "free"`。
	//
	// 这也是本表里唯一一个「关掉之后其它字段大多失去意义」的开关：
	// Sync 为 false 时 Snippets/Pages/Tags 只能由客户端自检（服务端看不到数据），
	// Devices 更是完全不构成约束（各设备之间本就互不相通）。
	Sync bool

	Snippets int
	Pages    int
	Tags     int

	// Devices 允许的同步设备数。Sync 为 false 时该值为 0，表示「不适用」而非「零台」——
	// 免费用户想在几台设备上装就装几台，只是数据不互通。UI 应显示「本地使用」，不要显示台数。
	Devices int

	// BindingsPerPlatform 是**每个平台**可绑定的账号数，不是总数。
	// Telegram / 微信 / 抖音各自独立计数（对应 platform_bindings 的泛化，ADR-075 待办 ⑩）。
	BindingsPerPlatform int

	// TrashDays 回收站保留天数。Sync 为 false 时为 0（回收站也只在本地）。
	TrashDays int

	HiddenPages   bool // 隐藏页面
	Rules         bool // 自动归类规则
	Import        bool // 从浏览器书签 / Pocket 等导入
	ParsePriority bool // 解析优先队列
}

// 档位键。这五个字符串同时是三家支付渠道的商品 ID 约定的一部分
// （Apple 按 `.` 拆 productID 取中段、Google 按 `_` 拆 `squyrrl_<tier>_<period>`、
// Stripe 读 metadata.squyrrl_tier），**改名等于同时废掉三个渠道的全部在售商品**。
const (
	Free     = "free"
	Basic    = "basic"
	Standard = "standard"
	Premium  = "premium"
	Maximum  = "maximum"
)

// Order 是档位由低到高的顺序，供比较与遍历使用。
var Order = []string{Free, Basic, Standard, Premium, Maximum}

var table = map[string]Tier{
	Free: {
		Key: Free, PriceUSDMonthly: 0,
		Sync:     false,
		Snippets: 1_000, Pages: 8, Tags: 16,
		Devices: 0, BindingsPerPlatform: 0, TrashDays: 0,
	},
	Basic: {
		Key: Basic, PriceUSDMonthly: 1,
		Sync:     true,
		Snippets: 10_000, Pages: 16, Tags: 32,
		Devices: 2, BindingsPerPlatform: 1, TrashDays: 30,
	},
	Standard: {
		Key: Standard, PriceUSDMonthly: 3,
		Sync:     true,
		Snippets: 100_000, Pages: 32, Tags: 64,
		Devices: 3, BindingsPerPlatform: 2, TrashDays: 90,
		HiddenPages: true,
	},
	Premium: {
		Key: Premium, PriceUSDMonthly: 6,
		Sync:     true,
		Snippets: 1_000_000, Pages: 64, Tags: 128,
		Devices: 4, BindingsPerPlatform: 4, TrashDays: 180,
		HiddenPages: true, Rules: true, Import: true,
	},
	Maximum: {
		Key: Maximum, PriceUSDMonthly: 12,
		Sync:     true,
		Snippets: 10_000_000, Pages: 128, Tags: 256,
		Devices: 5, BindingsPerPlatform: 8, TrashDays: 365,
		HiddenPages: true, Rules: true, Import: true, ParsePriority: true,
	},
}

// Of 取某档位的能力表。未知档位返回 false —— 调用方必须处理，
// 不要退化到 Free：一个拼错的 tier 悄悄降级成免费档，表现是付费用户功能全失而无任何报错。
func Of(tier string) (Tier, bool) {
	t, ok := table[tier]
	return t, ok
}

// Known 报告档位是否合法。webhook 侧用它校验来自三家支付渠道的 tier 字符串
// （那些值由商品 ID 或 metadata 解析而来，是外部输入）。
func Known(tier string) bool {
	_, ok := table[tier]
	return ok
}

// MustOf 取能力表，未知档位回落到 Free。
// 仅用于「已经确定用户没有 active plan」的读路径 —— wallet.ActivePlan 查不到订阅时就返回 "free"，
// 那是正常状态而非错误。写路径和校验路径一律用 Of 并处理 false。
func MustOf(tier string) Tier {
	if t, ok := table[tier]; ok {
		return t
	}
	return table[Free]
}

// Rank 返回档位在 Order 中的序号，未知档位返回 -1。
// 用于「这个功能至少需要 standard」这类比较。
func Rank(tier string) int {
	for i, k := range Order {
		if k == tier {
			return i
		}
	}
	return -1
}

// AtLeast 报告 tier 是否达到 min 档。未知档位一律返回 false（fail-closed）。
func AtLeast(tier, min string) bool {
	rt, rm := Rank(tier), Rank(min)
	return rt >= 0 && rm >= 0 && rt >= rm
}
