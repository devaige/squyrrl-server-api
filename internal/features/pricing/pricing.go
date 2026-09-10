// Package pricing 承载用户可见价格的换算规则。
//
// ADR-013 定下的原则是「用户侧售价从上游成本 + 毛利算出，而不是写死」，
// 这样供应商调价时不需要改客户端。ADR-075 把汇率从 1:1000 改成 1:10000，
// 并把毛利从一个全局常量下放到每个 endpoint（api_endpoints.margin_bp）。
//
// 这个包只做换算，不碰数据库 —— 便于单测覆盖边界，也让 GET /pricing
// 未来可以直接复用同一份公式，避免「客户端显示的价」和「实际扣的费」出现两套实现。
package pricing

import (
	"errors"
	"math"
)

// CreditsPerUSD 是代币与美元的固定兑换率（ADR-013，ADR-075 改为 10 000）。
//
// 为什么是 10 000 而不是 1 000：旧汇率下 1 credit = $0.001，恰好等于最便宜的
// 上游解析报价 —— 最小计价单位就是成本本身，于是任何毛利都无法表达
// （$0.0015 的 endpoint 加价 2 倍应是 3 credits，旧汇率下只能取整成 2 或 3，误差 33%）。
// 提高一个数量级换来的是定价精度，用户侧数字更大只是附带效果。
const CreditsPerUSD = 10_000

// MicrosPerCredit：上游成本以「百万分之一美元」计价，1 credit = $0.0001 = 100 micros。
const MicrosPerCredit = 1_000_000 / CreditsPerUSD // = 100

// DefaultMarginBP 是 endpoint 未单独配置时的加价倍率：2.0 倍，即 50% 毛利。
// 与 migration 000015 里 api_endpoints.margin_bp 的 DEFAULT 保持一致。
const DefaultMarginBP = 20_000

// MinCreditCost 是任何一次解析的最低收费。
//
// 内置 provider（YouTube oEmbed、GenericOG）上游成本为零，但它们仍然占用 CPU、
// 走出网请求，并让服务器承担被目标站判定为爬虫的风险 —— ADR-048 就是因为
// 服务端代抓网页被判爬虫才把整个归档功能删掉的。定价为零等于鼓励刷。
const MinCreditCost = 10

// BPDenominator 是万分比的分母。
const BPDenominator = 10_000

var (
	// ErrBadMargin 表示加价倍率不在合法区间。
	ErrBadMargin = errors.New("margin_bp out of range")
	// ErrCostOverflow 表示成本 × 倍率超出 int64，只可能来自配置错误或恶意写入。
	ErrCostOverflow = errors.New("credit cost computation overflows")
)

// MaxMarginBP 与 migration 里的 CHECK 约束一致（100 倍封顶）。
const MaxMarginBP = 1_000_000

// CreditCost 把一次上游调用的成本换算成向用户收取的代币数。
//
//	credit_cost = max(MinCreditCost, ceil(unitPriceMicros × marginBP / 1e6))
//
// 除数 1e6 = MicrosPerCredit(100) × BPDenominator(10000)，即「先把 micros 换成 credits，
// 再乘以倍率」两步合并成一次整数运算 —— 分两步做会在中间取整两次，
// 便宜的 endpoint 上误差可以到 50%。
//
// 全程整数、向上取整：宁可多收一个代币（$0.0001），也不要因为向下取整
// 在每次调用上少收，那种亏损随用量线性放大且账面看不见。
func CreditCost(unitPriceMicros int64, marginBP int32) (int64, error) {
	if marginBP <= 0 || marginBP > MaxMarginBP {
		return 0, ErrBadMargin
	}
	if unitPriceMicros < 0 {
		return 0, ErrBadMargin
	}
	if unitPriceMicros == 0 {
		return MinCreditCost, nil
	}

	// 溢出保护：现实取值（micros ~1e4、bp ~2e4）离 int64 上限差 10 个数量级，
	// 这里防的是配置写错或被写入极端值，而不是正常业务。
	if unitPriceMicros > math.MaxInt64/int64(marginBP) {
		return 0, ErrCostOverflow
	}

	const divisor = int64(MicrosPerCredit) * int64(BPDenominator) // 1e6
	product := unitPriceMicros * int64(marginBP)

	// 整数向上取整，不用 math.Ceil —— float64 只有 53 位尾数，
	// 大额 product 转成浮点会丢精度，而这里是钱。
	cost := (product + divisor - 1) / divisor

	if cost < MinCreditCost {
		return MinCreditCost, nil
	}
	return cost, nil
}

// USDToCredits 把美元金额（以 micros 计）换算成代币数，用于加购套餐的标价。
// 不加毛利 —— 加购是按面值售卖，毛利体现在消耗侧的 CreditCost 上。
func USDToCredits(usdMicros int64) int64 {
	return usdMicros / MicrosPerCredit
}
