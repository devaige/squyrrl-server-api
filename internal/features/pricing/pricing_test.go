package pricing

import (
	"errors"
	"math"
	"testing"
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
// 这才是「叠加安全」的准确表述。设计时说的是线性（$0.03/GB·月），但 1 TB / 2 TB
// 两档取整后单价略低（$0.293 vs $0.30）—— 方向是「买大档不吃亏」，无害。
// 真正要防的是反过来：某个大档单价更高，导致拆成小档叠加更便宜，
// 那种价目表会被用户当成陷阱，而且是完全正当的指责。
func TestStorageUnitPriceNeverIncreases(t *testing.T) {
	for i := 1; i < len(storageTiers); i++ {
		lo, hi := storageTiers[i-1], storageTiers[i]
		if hi.GB <= lo.GB {
			t.Fatalf("档位未按容量升序：%d GB 出现在 %d GB 之后", hi.GB, lo.GB)
		}
		// 比较 lo.Price/lo.GB >= hi.Price/hi.GB，用交叉相乘避免浮点
		if lo.PriceUSDYear*hi.GB < hi.PriceUSDYear*lo.GB {
			t.Errorf("%d GB 的单价高于 %d GB —— 拆成小档叠加会更便宜",
				hi.GB, lo.GB)
		}
	}
}

// 叠加买到的容量，价格不应低于直接买同等的单档（否则单档就没有存在意义）。
func TestStackingIsNeverCheaperThanASingleTier(t *testing.T) {
	for _, target := range storageTiers {
		// 用最便宜的单位价档位去凑 target.GB
		best := storageTiers[0]
		n := (target.GB + best.GB - 1) / best.GB
		stacked := n * best.PriceUSDYear
		if stacked < target.PriceUSDYear {
			t.Errorf("%d GB 单档 $%d，用 %d 份 %d GB 叠加只要 $%d",
				target.GB, target.PriceUSDYear, n, best.GB, stacked)
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
		{19, 0}, // 不足最低档
		{20, 2 * gb},
		{100, 2 * gb},
		{200, 10 * gb},
		{1023, 10 * gb},
		{1024, 50 * gb},
		{5000, 50 * gb}, // 超出最高档仍取最高档
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
