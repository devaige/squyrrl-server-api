package pricing

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// Catalog 是 GET /pricing 的响应：三块商品的完整价目。
//
// 它是**唯一能不发版调价的机制**（ADR-013 的原始意图，ADR-075 扩展到三块）。
// 客户端冷启动拉一次，用它渲染购买页、算「帮我选择」向导的推荐组合、
// 以及在解析前告诉用户这次要花多少代币。
type Catalog struct {
	// CreditsPerUSD 汇率。客户端展示金额时反向换算用。
	CreditsPerUSD int `json:"credits_per_usd"`

	// Plans 五个基础订阅档位，按由低到高排列 —— 顺序即展示顺序，
	// 客户端不该自己排序（价格相同的档位排序会不稳定）。
	Plans []entitlement.Tier `json:"plans"`

	// YearlyMonths 年付大约相当于几个月的价格，**只用于展示**（「省两个月」）。
	//
	// 不要再拿它去乘出年付价：商店的价格点让实际年价落在 $19.99 而不是
	// $19.90，两者差 9 分，而客户端一旦自己乘一遍，显示的价格就和扣款对不上。
	// 权威年价是 entitlement.Tier.PriceCentsYearly。
	YearlyMonths int `json:"yearly_months"`

	// Storage 云存储档位。**仅月付**（2026-09-16 用户决策，推翻了原先的仅年付）。
	//
	// 换成月付是因为年付把大档的单期金额推出了商店的价格点范围：10 TB 年付
	// 需要 $3000 上下，而自动续期订阅的价格点到 $999.99 为止 —— 那一档在
	// App Store Connect 里根本建不出来。月付把同一个价格摊成 1/12，上限不再
	// 是约束，代价见 [storageTiers] 上那条关于站外渠道的警告。
	Storage []StorageTier `json:"storage"`

	// CreditPacks 代币加购档位，一次性购买、永不过期。
	CreditPacks []CreditPack `json:"credit_packs"`

	// SignupGrantCredits 注册赠额。客户端拿它当「代币是否该补」的水位线：
	// 余额低于新手额度，就说明赠额已经用得差不多了。
	// 发出来而不是让客户端自己写一个常量 —— 那个数字改一次，两侧就分了家，
	// 而分家的表现只是推荐组合悄悄变得不合适，没有任何报错。
	SignupGrantCredits int64 `json:"signup_grant_credits"`

	// MaxFileSize 单文件上限随总存储配额变化，不是订阅属性。
	// 做成阶梯而非订阅字段，是因为存储可叠加，写成字段就要回答
	// 「叠加时取最大档还是取总和」这个没有好答案的问题。
	MaxFileSize []FileSizeTier `json:"max_file_size"`
}

type StorageTier struct {
	GB int `json:"gb"`
	// PriceCentsMonthly 月付价（美分）。单位与 entitlement.Tier 一致，理由同上。
	PriceCentsMonthly int `json:"price_cents_monthly"`

	// NativeOnly 表示这一档**只在商店内购里上架**，站外结账不卖。
	//
	// 不是商业偏好，是算术：Stripe 每笔收 2.9% + $0.30 的**固定**费，而固定
	// 那部分不随金额缩小。10 GB 档卖 $0.29，扣完手续费到手是负数 —— 卖一份
	// 亏一份；20 GB 档到手 $0.176，不够付 $0.30 的存储成本。商店按比例抽成，
	// 没有固定项，所以同样两档在内购通路上分别是 135% 和 114% 的保本占用率。
	//
	// 表达成「哪一档不卖」而不是「Stripe 上另一套价格」：同一容量两个价钱，
	// 用户在网页和 App 里看到的数字对不上，那是要写一整段解释的东西。
	// 判据锁在 [TestStripeEligibleTiersBreakEven]。
	NativeOnly bool `json:"native_only,omitempty"`
}

type CreditPack struct {
	// PriceUSD 是**面值档**，只用来生成商品键（`p1` / `p5`）和标注这一档「值多少钱」。
	// 它不是收款额 —— 见 [PriceCents]。
	PriceUSD int `json:"price_usd"`

	// PriceCents 实收价（美分）。
	//
	// 与面值差 1 分是被商店的价格点逼出来的：App Store 的消耗型和订阅共用一套
	// 价格点，而那套点位没有整数美元，$1 只能落到 $0.99。跨渠道同价（2026-09-16
	// 用户决策）意味着 Stripe 也跟着收 $0.99，否则网页和 App 里同一个包两个价。
	//
	// 少收的那 1 分**不影响发放量**：到账代币由 [CreditsForPackKey] 查表决定，
	// 与商店收了多少钱无关。代价是每档让出约 1% 的毛利，在 $1 档上把 2.0 倍的
	// 加价压到约 1.32 倍（站外还要再扣 $0.30 固定费）—— 最小档本来就是引流档。
	PriceCents int `json:"price_cents"`

	Credits int64 `json:"credits"`
}

type FileSizeTier struct {
	MinQuotaGB   int   `json:"min_quota_gb"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}

const (
	// YearlyMonths 年付按 10 个月计价。
	YearlyMonths = 10
	// SignupGrantCredits 注册赠送额度，够 3 条最贵的解析（200 × 3）。
	// 它属于获客成本，不是任何一档订阅的赠品 —— 三块商品互相独立。
	SignupGrantCredits = 600
)

// 档位宽限期：扣款失败后仍按**原档位**服务的窗口（ADR-075 ⑭）。
//
// 这一层管的是「支付还在重试」，与降级之后的数据生命周期（30 天宽限 + 30 天冻结）
// 是两件先后发生的事：这里的窗口走完仍未续上，才真正降级并启动那条链路。
// 一张卡两天后补上，不该让用户看到任何可见的状态翻转 —— 那正是这一层存在的理由。
//
// 长度随计费周期变（用户决策 2026-09-11）：周期越长，用户越不会天天盯着账单，
// 察觉扣款失败所需的时间也越长。
//
// **这只是默认值。** Apple 与 Google 的 billing grace period 由商店后台按产品配置，
// 商店通知里带的截止时间才是权威；接 IAP 时 SubscriptionEvent 要带上它并优先采用。
// 放在 pricing 而不是 wallet：quota 也要用（存储配额同样吃宽限期），
// 而 quota → wallet 会成环（wallet 的 handler 已依赖 quota）。
const (
	GraceMonthly = 7 * 24 * time.Hour
	GraceYearly  = 14 * 24 * time.Hour
)

// GraceFor 返回某计费周期对应的档位宽限期。
// 未知周期按月付处理 —— 该保守的一侧是给得少，而不是给一个我们没定义过的长度。
func GraceFor(billingPeriod string) time.Duration {
	if billingPeriod == "yearly" {
		return GraceYearly
	}
	return GraceMonthly
}

// storageTiers 每档容量翻倍，价格贴着「抽成 30% 后仍不亏」这条地板定。
//
// **线性在这里换了一条理由，结论没变。** 原先它是「可叠加」的前提（5 份 20 GB
// 与 1 份 100 GB 必须同价，阶梯价会让前者更贵，用户有理由认为那是陷阱）。
// 叠加后来被证明在两条原生通路上都做不到 —— App Store 与 Google Play 都拒绝
// 重复购买同一个订阅商品（Play 直接回 ITEM_ALREADY_OWNED）——那条理由随之消失。
//
// 现在支撑它的是**保本占用率**（2026-09-16 用户决策：按商店抽成 30% 的最坏情形，
// 每一档都要 ≥100%）。成本严格随容量线性（R2 标准存储 $0.015/GB·月），所以
// 「配额被填满时也不亏」这个要求等价于「单价有一条统一的下限」：
// price ≥ GB × 0.015 / 0.7 = GB × $0.0214。单价一旦随容量递减到这条线以下，
// 大档卖的就不再是空间，而是「用户不会把买到的空间用完」这个赌注 ——
// 而那个赌注需要一个占用率中位数来支撑，那个数字没有任何权威公开来源。
//
// **最小档是 10 GB。** App Store 的最低价格点是 $0.29 而不是 $0.99（2026-09-16
// 更正：控制台默认只展示常用价位，全表里还有更低的）。$0.29 在保本线上覆盖
// 13 GB，所以 10 GB 是能建出来的最小 ×2 档位。
//
// 单价因此不是一个常数（$0.029 → $0.0215）：价格点只有 .99 / .49 / .29 这种粒度，
// 贴着地板向上取整，容量越大取整损失的占比越小。方向是对的 —— 大档更便宜。
//
// 档位序列是严格 ×2（2026-09-16），显示除数也跟着回到 1024，
// 于是 10240 GB 正好显示成「10 TB」。改 SKU 键正常是禁止的（会让每一条存量
// 订阅的续期事件变成未知档位），这次可以改只因为**还没有任何渠道上架过存储
// 商品** —— 上架第一个就关窗。
//
// **最小的两档只在内购上卖**（[StorageTier.NativeOnly]）。这是本表唯一一处
// 跨渠道差异，理由是 Stripe 的固定手续费在小额上吃掉全部毛利，推导见那个字段。
//
// ⚠️ 真正没有余量的是**各区价格表**：Apple 不按汇率等值换算，同一个价格点在
// 低价区的美元等值可能只有七八成。这张表按美国区刚好 100% 保本，
// 那些区就在 100% 以下。真要留余量，第一个该加的地方在这里，不是大档。
var storageTiers = []StorageTier{
	{GB: 10, PriceCentsMonthly: 29, NativeOnly: true},
	{GB: 20, PriceCentsMonthly: 49, NativeOnly: true},
	{GB: 40, PriceCentsMonthly: 99},
	{GB: 80, PriceCentsMonthly: 199},
	{GB: 160, PriceCentsMonthly: 399},
	{GB: 320, PriceCentsMonthly: 699},
	{GB: 640, PriceCentsMonthly: 1399},
	{GB: 1280, PriceCentsMonthly: 2799},
	{GB: 2560, PriceCentsMonthly: 5499},
	{GB: 5120, PriceCentsMonthly: 10999},
	{GB: 10240, PriceCentsMonthly: 21999},
}

// StorageTierKey 是存储档位在三家支付渠道里的商品标识后缀：20 GB → "s20"。
//
// 用容量本身当键，而不是另起一套 tier1/tier2：后者要求所有人都记住
// 「tier3 是多少 GB」，而那个映射只存在于某个文件里。容量是用户、运营、
// 支付后台三方都直接认得的东西。
func StorageTierKey(gb int) string { return fmt.Sprintf("s%d", gb) }

// StorageGBForKey 由商品标识反查容量，未知档位返回 false。
//
// **必须是查表而不是从 "s50" 里 parse 出 50**：那样任何人在支付后台建一个
// "s999" 的商品都能凭空创造配额，而 webhook 会照单全收。查表意味着
// 我们只认自己上架过的档位。
func StorageGBForKey(key string) (int, bool) {
	for _, t := range storageTiers {
		if StorageTierKey(t.GB) == key {
			return t.GB, true
		}
	}
	return 0, false
}

// StorageTierByKey 由商品标识反查整档，供调用方自己判断通路限制
// （[StorageTier.NativeOnly]）。
//
// 与 [StorageGBForKey] 并存而不是取而代之：原生内购那条路认全部档位，
// 站外结账要多问一句「这档卖不卖」。把选择权交给调用方，而不是在这里塞一个
// 通路参数 —— 那样每加一条通路就要改这个函数的签名和每一个调用点。
func StorageTierByKey(key string) (StorageTier, bool) {
	for _, t := range storageTiers {
		if StorageTierKey(t.GB) == key {
			return t, true
		}
	}
	return StorageTier{}, false
}

// CreditPackKey 是代币加购在支付渠道里的商品标识后缀：$5 档 → "p5"。
// 与存储同样用面值当键，理由相同：面值是三方都直接认得的东西。
func CreditPackKey(usd int) string { return fmt.Sprintf("p%d", usd) }

// CreditsForPackKey 由商品标识反查代币数，未知档位返回 false。
// 与 StorageGBForKey 同理：**查表而不是从键里 parse**，否则任何人在支付后台
// 建一个 "p9999" 的商品就能凭空创造代币。
func CreditsForPackKey(key string) (int64, bool) {
	for _, p := range creditPacks {
		if CreditPackKey(p.PriceUSD) == key {
			return p.Credits, true
		}
	}
	return 0, false
}

// creditPacks 买得多送得多。面值按 CreditsPerUSD 折算，加成写在 Credits 里。
//
// 赠额每档 +5%，是为了让**加成本身成为一条可读的规律**而不是三个孤立的数字：
// 用户看到 $3 送 5%、$5 送 10%，不用算就知道再往上一档还会更划算。
// 客户端不另发「赠额」字段，它 = Credits − PriceUSD × CreditsPerUSD，
// 两边各算各的就会在改价那天分家。
//
// **面值与实收价差 1 分**：商店的价格点没有整数美元，$1 只能落到 $0.99，
// 而跨渠道同价要求 Stripe 跟着收同样的数。赠额仍按面值算，理由见 [CreditPack.PriceCents]。
var creditPacks = []CreditPack{
	{PriceUSD: 1, PriceCents: 99, Credits: 10_000},     // 面值，无赠额
	{PriceUSD: 3, PriceCents: 299, Credits: 31_500},    // +5%
	{PriceUSD: 5, PriceCents: 499, Credits: 55_000},    // +10%
	{PriceUSD: 10, PriceCents: 999, Credits: 115_000},  // +15%
	{PriceUSD: 20, PriceCents: 1999, Credits: 240_000}, // +20%
	{PriceUSD: 50, PriceCents: 4999, Credits: 625_000}, // +25%
}

// fileSizeTiers 单文件上限。
//
// 硬顶是 78 GB：分片恒 8 MiB，而 S3/R2 的 multipart 最多 10 000 片
// （8 MiB × 10000 = 78.125 GB），且分片大小被 ADR-069 夹在 R2 的 5 MiB 下限
// 与 Cloudflare 按账户 plan 计的请求体上限之间，不能随意调大。
// 50 GB 留了 36% 余量，**不要把这个数字往 78 GB 附近抬**。
// 阈值必须落在 storageTiers 真实存在的档位上，否则某一档的买家会够不到他
// 刚买下的那级上限。
//
// **最低那条必须跟着最小档走**：10 GB / 20 GB 补回来之后若最低阈值还停在 40，
// `MaxFileBytesFor` 对这两档返回 0，而 0 的语义是「没买存储，不许上传」——
// 表现是用户刚买完存储、一个文件也传不了，且错误信息说的是「请先购买存储」。
var fileSizeTiers = []FileSizeTier{
	{MinQuotaGB: 10, MaxFileBytes: 1 << 30},   // 1 GB，入门两档
	{MinQuotaGB: 40, MaxFileBytes: 2 << 30},   // 2 GB
	{MinQuotaGB: 160, MaxFileBytes: 10 << 30}, // 10 GB
	{MinQuotaGB: 1280, MaxFileBytes: 50 << 30},
}

// MaxFileBytesFor 按总配额算出单文件上限。配额为 0（未购买存储）时返回 0，
// 表示不允许上传任何文件。
func MaxFileBytesFor(quotaGB int) int64 {
	var max int64
	for _, t := range fileSizeTiers {
		if quotaGB >= t.MinQuotaGB {
			max = t.MaxFileBytes
		}
	}
	return max
}

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

// RegisterPublic 挂在公开 group 上 —— 无 Bearer。
//
// 定价必须匿名可读：未登录用户要能看购买页，「帮我选择」向导也在登录前就要能算。
// 与 GET /uris/manifest 同一个模式（ADR-061）。
func (h *Handler) RegisterPublic(g *gin.RouterGroup) {
	g.GET("/pricing", h.get)
}

func (h *Handler) get(c *gin.Context) {
	plans := make([]entitlement.Tier, 0, len(entitlement.Order))
	for _, k := range entitlement.Order {
		if t, ok := entitlement.Of(k); ok {
			plans = append(plans, t)
		}
	}
	// 缓存 5 分钟：价目变动不频繁，而这个端点会被每个冷启动打一次。
	// 与 /uris/manifest 取同一个值，两者的变更节奏相当。
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, Catalog{
		CreditsPerUSD: CreditsPerUSD,
		Plans:         plans,
		YearlyMonths:  YearlyMonths,
		Storage:       storageTiers,
		CreditPacks:   creditPacks,
		MaxFileSize:   fileSizeTiers,

		SignupGrantCredits: SignupGrantCredits,
	})
}
