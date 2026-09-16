package pricing

import (
	"errors"
	"math"
	"testing"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// ADR-075 里写死的三档参考价必须能被公式复现，否则文档与实现已经分家。
func TestCreditCostMatchesADR(t *testing.T) {
	cases := []struct {
		name   string
		micros int64 // 上游单价
		want   int64
	}{
		{"上游 $0.001", 1_000, 20},
		{"上游 $0.005", 5_000, 100},
		{"上游 $0.01", 10_000, 200},
		{"内置免费 provider", 0, MinCreditCost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CreditCost(c.micros, DefaultMarginBP)
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != c.want {
				t.Errorf("售价 %d credits，期望 %d", got, c.want)
			}
		})
	}
}

// 向上取整必须真的向上：任何非零成本都不能收到比成本还少的钱。
func TestCreditCostRoundsUp(t *testing.T) {
	// 1 micro 成本 × 2.0 倍 = 0.00002 credits，向上取整后受 MinCreditCost 兜底
	got, err := CreditCost(1, DefaultMarginBP)
	if err != nil {
		t.Fatal(err)
	}
	if got != MinCreditCost {
		t.Errorf("极小成本应落到下限 %d，实际 %d", MinCreditCost, got)
	}

	// 刚好越过下限的场景：需要 product/1e6 > 10，即 micros × bp > 1e7
	// micros=501, bp=20000 → 1.002e7 → ceil(10.02) = 11
	got, err = CreditCost(501, DefaultMarginBP)
	if err != nil {
		t.Fatal(err)
	}
	if got != 11 {
		t.Errorf("期望 11（向上取整），实际 %d", got)
	}

	// 若实现里用了向下取整，这里会得到 10 而不是 11 —— 每次调用少收一点，
	// 亏损随用量线性放大且账面上看不见，正是这条断言要钉死的。
}

// 下限对所有倍率生效，而不只是默认倍率。
func TestMinCostAppliesAtAnyMargin(t *testing.T) {
	for _, bp := range []int32{1, 100, DefaultMarginBP, MaxMarginBP} {
		got, err := CreditCost(0, bp)
		if err != nil {
			t.Fatalf("bp=%d 不应报错：%v", bp, err)
		}
		if got != MinCreditCost {
			t.Errorf("bp=%d 时零成本应收 %d，实际 %d", bp, MinCreditCost, got)
		}
	}
}

func TestCreditCostRejectsBadInput(t *testing.T) {
	bad := []struct {
		name   string
		micros int64
		bp     int32
	}{
		{"倍率为零", 1000, 0},
		{"倍率为负", 1000, -1},
		{"倍率超上限", 1000, MaxMarginBP + 1},
		{"成本为负", -1, DefaultMarginBP},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := CreditCost(c.micros, c.bp); !errors.Is(err, ErrBadMargin) {
				t.Errorf("期望 ErrBadMargin，实际 %v", err)
			}
		})
	}
}

// 溢出必须报错而不是回绕成一个荒谬的小数字 —— 回绕后的售价可能低于成本。
func TestCreditCostOverflow(t *testing.T) {
	if _, err := CreditCost(math.MaxInt64, DefaultMarginBP); !errors.Is(err, ErrCostOverflow) {
		t.Errorf("期望 ErrCostOverflow，实际 %v", err)
	}
	if _, err := CreditCost(math.MaxInt64/int64(DefaultMarginBP)+1, DefaultMarginBP); !errors.Is(err, ErrCostOverflow) {
		t.Error("刚越过边界的值应判定溢出")
	}
	// 边界内侧应当正常算出
	if _, err := CreditCost(math.MaxInt64/int64(DefaultMarginBP), DefaultMarginBP); err != nil {
		t.Errorf("边界内不应报错：%v", err)
	}
}

// 汇率是三处硬编码的交汇点：官网文案、客户端展示、服务端扣费。
// 常量之间的关系写成断言，改一个忘了另一个时会红。
func TestRateConstantsAreConsistent(t *testing.T) {
	if CreditsPerUSD*MicrosPerCredit != 1_000_000 {
		t.Errorf("汇率与 micros 换算不自洽：%d × %d ≠ 1e6", CreditsPerUSD, MicrosPerCredit)
	}
	if USDToCredits(1_000_000) != CreditsPerUSD {
		t.Errorf("$1 应换 %d credits，实际 %d", CreditsPerUSD, USDToCredits(1_000_000))
	}
	// ADR-075 的加购档位：$1 = 10 000
	if got := USDToCredits(1_000_000); got != 10_000 {
		t.Errorf("ADR-075 定 $1 = 10 000 credits，实际 %d", got)
	}
}

// 存储定价的核心不变量：**单位价格随档位增大不上升**。
//
// 这才是「叠加安全」的准确表述。档位改成十进制（1000/2000）之后整张表恰好
// 严格线性 $0.30/GB·年，但断言仍写成「不上升」而不是「相等」：未来给大档让点
// 利是合理的商业动作，而反过来 —— 某个大档单价更高、拆成小档叠加反而便宜 ——
// 是用户完全有正当理由称之为陷阱的那一种，那才是这条测试要挡住的。
// 容量必须升序，且**最大档的单价不得高于最小档** —— 买得多反而更贵，
// 是用户有正当理由称之为陷阱的那一种。
//
// 这里刻意只比首尾，不要求逐档单调：价格点只有 .99 这一种粒度，贴着保本线
// 向上取整会让相邻档之间出现零点几个百分点的起伏（$0.99/40 GB 与 $1.99/80 GB
// 差 0.05%）。那种起伏不构成套利，真正的判据在
// [TestStackingIsNeverCheaperThanASingleTier] —— 它算的是用户实际要掏的钱。
func TestStorageTiersAscendAndLargestIsNotPricierPerGB(t *testing.T) {
	for i := 1; i < len(storageTiers); i++ {
		if storageTiers[i].GB <= storageTiers[i-1].GB {
			t.Fatalf("档位未按容量升序：%d GB 出现在 %d GB 之后",
				storageTiers[i].GB, storageTiers[i-1].GB)
		}
	}
	first, last := storageTiers[0], storageTiers[len(storageTiers)-1]
	// last.Price/last.GB <= first.Price/first.GB，交叉相乘避免浮点
	if last.PriceCentsMonthly*first.GB > first.PriceCentsMonthly*last.GB {
		t.Errorf("最大档 %d GB 的单价高于最小档 %d GB", last.GB, first.GB)
	}
}

// 每一档在配额被**填满**时都必须仍然盈利（2026-09-16 用户决策）。
//
// 这是整张表的地板，也是它必须线性的原因：R2 的成本严格随容量线性，
// 单价一旦随容量递减，大档的保本点就掉到 100% 占用率以下 —— 那时卖的不再是
// 空间，而是「用户不会把买到的空间用完」这个赌注，而大容量买家恰恰最可能用满。
//
// 按商店抽成 **30%** 算，即尚未加入 App Store 小企业计划的情形。加入之后抽 15%，
// 余量只会更大 —— 这里锁住的是最坏的那一侧。
func TestStorageBreaksEvenAtFullOccupancy(t *testing.T) {
	// R2 标准存储 $0.015/GB·月 = 1.5 分。
	const r2CentsPerGBMonth = 1.5
	const storeCut = 0.30

	for _, tier := range storageTiers {
		net := float64(tier.PriceCentsMonthly) * (1 - storeCut)
		cost := float64(tier.GB) * r2CentsPerGBMonth
		if net < cost {
			t.Errorf("%d GB：满配额成本 %.1f 分，抽成后到手 %.1f 分 —— 填满就亏",
				tier.GB, cost, net)
		}
	}
}

// 拆成小档凑出同等容量，不应该比直接买单档便宜。
//
// **判据带上 Stripe 每笔 $0.30 的固定手续费**，因为那才是用户实际掏的钱 ——
// 而且只有 Stripe 这一条通路真的拆得开：两家原生商店都拒绝重复购买同一个
// 订阅商品（Play 直接回 ITEM_ALREADY_OWNED）。不算这笔费用的话，.99 价格点
// 带来的那一两分钱起伏会让这条测试误报：2 × $0.99 = $1.98 看着比 $1.99 便宜，
// 但真付起来是 $1.98 加两笔手续费，比单档贵 $0.29。
func TestStackingIsNeverCheaperThanASingleTier(t *testing.T) {
	// Stripe: 2.9% + $0.30。这里只算固定部分 —— 百分比部分对两侧同比例作用，
	// 不影响谁更便宜。
	const stripeFixedFeeCents = 30

	for _, target := range storageTiers {
		// 用最便宜的单位价档位去凑 target.GB
		best := storageTiers[0]
		n := (target.GB + best.GB - 1) / best.GB
		stacked := n*best.PriceCentsMonthly + (n-1)*stripeFixedFeeCents
		if stacked < target.PriceCentsMonthly {
			t.Errorf("%d GB 单档 %d 分，用 %d 份 %d GB 叠加只要 %d 分（含手续费）",
				target.GB, target.PriceCentsMonthly, n, best.GB, stacked)
		}
	}
}

func TestMaxFileBytesFor(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		quotaGB int
		want    int64
	}{
		{0, 0},  // 未购买存储：不允许上传
		{39, 0}, // 不足最低档
		{40, 2 * gb},
		{80, 2 * gb},
		{160, 10 * gb},
		{1279, 10 * gb},
		{1280, 50 * gb},
		{10240, 50 * gb}, // 超出最高档仍取最高档
	}
	for _, c := range cases {
		if got := MaxFileBytesFor(c.quotaGB); got != c.want {
			t.Errorf("配额 %d GB 的单文件上限 = %d，期望 %d", c.quotaGB, got, c.want)
		}
	}
}

// 单文件上限的硬顶来自 multipart：8 MiB × 10000 片 = 78.125 GB。
// 任何一档越过它，上传会在最后一片失败 —— 而那时用户已经传了几十 GB。
func TestFileSizeTiersStayUnderMultipartCeiling(t *testing.T) {
	const ceiling = int64(8) << 20 * 10000 // 8 MiB × 10000 parts
	for _, ft := range fileSizeTiers {
		if ft.MaxFileBytes >= ceiling {
			t.Errorf("%d GB 档的单文件上限 %d 越过 multipart 硬顶 %d",
				ft.MinQuotaGB, ft.MaxFileBytes, ceiling)
		}
	}
}

// 加购档位必须「买得多不吃亏」：每美元换到的代币数不递减。
func TestCreditPacksNeverGetWorse(t *testing.T) {
	for i := 1; i < len(creditPacks); i++ {
		lo, hi := creditPacks[i-1], creditPacks[i]
		if hi.PriceUSD <= lo.PriceUSD {
			t.Fatalf("加购档位未按价格升序")
		}
		// lo.Credits/lo.Price <= hi.Credits/hi.Price
		if lo.Credits*int64(hi.PriceUSD) > hi.Credits*int64(lo.PriceUSD) {
			t.Errorf("$%d 档每美元换到的代币少于 $%d 档", hi.PriceUSD, lo.PriceUSD)
		}
	}
	// 最小档必须严格等于面值，否则汇率就不是 $1 = 10000 了
	if creditPacks[0].Credits != int64(creditPacks[0].PriceUSD)*CreditsPerUSD {
		t.Errorf("$%d 档应换 %d 代币（面值），实际 %d",
			creditPacks[0].PriceUSD, int64(creditPacks[0].PriceUSD)*CreditsPerUSD, creditPacks[0].Credits)
	}
}

// 注册赠送必须够 3 条最贵的解析 —— 承诺是「3 条」，就要在最坏 provider 下也成立。
func TestSignupGrantCoversThreeExpensiveParses(t *testing.T) {
	const priciestUpstreamMicros = 10_000 // $0.01
	cost, err := CreditCost(priciestUpstreamMicros, DefaultMarginBP)
	if err != nil {
		t.Fatal(err)
	}
	if SignupGrantCredits < cost*3 {
		t.Errorf("赠送 %d 代币，但 3 条最贵解析需要 %d", SignupGrantCredits, cost*3)
	}
}

// TestSellableSKUCountMatchesConsole 锁住「要在商店后台手工建几个商品」这个数。
//
// 商品号在 App Store Connect / Play Console 里是手工建的，没有任何一端能自动同步
// （清单在 docs/readme/14-iap.md 附录 A）。加一档存储、加一个代币包都编得过、
// 测得过，代价要到用户点下「购买」、商店回「查无此商品」时才出现 ——
// 而那时新档位已经画在商品页上了。用一个数把这件事挡在 CI 上。
//
// 数字本身没有含义，改档位时连同附录 A 一起改。
func TestSellableSKUCountMatchesConsole(t *testing.T) {
	paidPlans := 0
	for _, k := range entitlement.Order {
		if tier, ok := entitlement.Of(k); ok && tier.PriceCentsMonthly > 0 {
			paidPlans++
		}
	}

	// plan 卖月付与年付两种周期；storage 只有月付；credits 是一次性。
	got := paidPlans*2 + len(storageTiers) + len(creditPacks)
	const want = 23
	if got != want {
		t.Fatalf("可售商品 %d 个，登记的是 %d 个 —— docs/readme/14-iap.md 附录 A 要跟着改", got, want)
	}
}
