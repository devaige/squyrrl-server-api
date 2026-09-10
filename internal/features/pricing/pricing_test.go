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
