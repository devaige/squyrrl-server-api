package pricing

import (
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

	// YearlyMonths 年付相当于几个月的价格。10 表示省两个月。
	YearlyMonths int `json:"yearly_months"`

	// Storage 云存储档位。**仅年付**：存储是唯一的累积型成本，
	// 预收一年正好对冲一年的字节支出，也把小额支付的固定手续费摊薄。
	Storage []StorageTier `json:"storage"`

	// CreditPacks 代币加购档位，一次性购买、永不过期。
	CreditPacks []CreditPack `json:"credit_packs"`

	// MaxFileSize 单文件上限随总存储配额变化，不是订阅属性。
	// 做成阶梯而非订阅字段，是因为存储可叠加，写成字段就要回答
	// 「叠加时取最大档还是取总和」这个没有好答案的问题。
	MaxFileSize []FileSizeTier `json:"max_file_size"`
}

type StorageTier struct {
	GB           int `json:"gb"`
	PriceUSDYear int `json:"price_usd_year"`
}

type CreditPack struct {
	PriceUSD int   `json:"price_usd"`
	Credits  int64 `json:"credits"`
}

type FileSizeTier struct {
	MinQuotaGB   int   `json:"min_quota_gb"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}

const (
	// GracePeriod 是订阅转入 past_due 之后仍按原档位服务的窗口（ADR-075 ⑭）。
	//
	// 取 14 天而不是 7 天：两侧代价不对称。多给一周，成本是一周的服务；少给一周，
	// 一个在外旅行、没看到催缴邮件的付费用户会发现自己的碎片「不见了」——
	// ADR-075 ⑧ 之后掉档意味着客户端切回本地模式，云端数据当场离开视野。
	// 绝大多数订阅中断本就是扣款失败而非主动取消，宁可多送一周。
	//
	// 放在 pricing 而不是 wallet：quota 也要用它（存储配额同样吃宽限期），
	// 而 quota → wallet 会成环（wallet 的 handler 已经依赖 quota）。
	GracePeriod = 14 * 24 * time.Hour

	// YearlyMonths 年付按 10 个月计价。
	YearlyMonths = 10
	// SignupGrantCredits 注册赠送额度，够 3 条最贵的解析（200 × 3）。
	// 它属于获客成本，不是任何一档订阅的赠品 —— 三块商品互相独立。
	SignupGrantCredits = 600
)

// storageTiers 按 $0.03/GB·月 × 10 个月定价，线性。
// 线性是叠加安全的前提：5 份 20 GB 与 1 份 100 GB 严格同价，
// 阶梯价会让前者更贵，用户有理由认为那是陷阱。
var storageTiers = []StorageTier{
	{GB: 20, PriceUSDYear: 6},
	{GB: 50, PriceUSDYear: 15},
	{GB: 100, PriceUSDYear: 30},
	{GB: 500, PriceUSDYear: 150},
	{GB: 1024, PriceUSDYear: 300},
	{GB: 2048, PriceUSDYear: 600},
}

// creditPacks 买得多送得多。面值按 CreditsPerUSD 折算，加成写在 Credits 里。
var creditPacks = []CreditPack{
	{PriceUSD: 1, Credits: 10_000},
	{PriceUSD: 5, Credits: 55_000},   // +10%
	{PriceUSD: 20, Credits: 240_000}, // +20%
}

// fileSizeTiers 单文件上限。
//
// 硬顶是 78 GB：分片恒 8 MiB，而 S3/R2 的 multipart 最多 10 000 片
// （8 MiB × 10000 = 78.125 GB），且分片大小被 ADR-069 夹在 R2 的 5 MiB 下限
// 与 Cloudflare 按账户 plan 计的请求体上限之间，不能随意调大。
// 50 GB 留了 36% 余量，**不要把这个数字往 78 GB 附近抬**。
var fileSizeTiers = []FileSizeTier{
	{MinQuotaGB: 20, MaxFileBytes: 2 << 30},   // 2 GB
	{MinQuotaGB: 200, MaxFileBytes: 10 << 30}, // 10 GB
	{MinQuotaGB: 1024, MaxFileBytes: 50 << 30},
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
	})
}
