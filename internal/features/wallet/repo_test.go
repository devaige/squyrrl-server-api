package wallet

import (
	"errors"
	"math"
	"testing"
)

func TestApplyDelta(t *testing.T) {
	cases := []struct {
		name    string
		curBal  int64
		delta   int64
		want    int64
		wantErr error
	}{
		{"普通入账", 1000, 500, 1500, nil},
		{"普通扣减", 1000, -400, 600, nil},
		{"扣到零是允许的", 1000, -1000, 0, nil},
		{"零余额入账", 0, 10000, 10000, nil},

		// 方案 A：拒绝而不是夹到 0
		{"扣超一点即拒绝", 1000, -1001, 0, ErrNegativeBalance},
		{"零余额上扣减", 0, -1, 0, ErrNegativeBalance},
		{"后台手滑填大负数", 500, -1_000_000, 0, ErrNegativeBalance},

		// 回绕：正 delta 溢出成负数，若不单独挡就会被误判为「余额变负」而报错误的原因；
		// 负 delta 下溢成正数更糟 —— 它会直接穿过 newBal < 0 的检查落账成功。
		{"正向溢出", math.MaxInt64, 1, 0, ErrGrantOverflow},
		{"负向下溢", -1, math.MinInt64, 0, ErrGrantOverflow},
		{"极大正 delta", 1, math.MaxInt64, 0, ErrGrantOverflow},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := applyDelta(c.curBal, c.delta)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("期望错误 %v，实际 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错，实际 %v", err)
			}
			if got != c.want {
				t.Errorf("余额 %d，期望 %d", got, c.want)
			}
		})
	}
}

// 负 delta 下溢是这组检查里唯一「不挡就会静默写坏账本」的一条：
// 它绕过负余额判断后会以一个巨大的正数落账，等于凭空发币。单独固化。
func TestUnderflowCannotMintCredits(t *testing.T) {
	_, err := applyDelta(-1, math.MinInt64)
	if !errors.Is(err, ErrGrantOverflow) {
		t.Fatalf("下溢必须被拒绝，实际 err=%v", err)
	}
	// 固化「不挡会怎样」：走变量而非常量表达式 —— 常量溢出是编译错误，
	// 只有运行时才会真的回绕，而回绕的结果是个正数，正是凭空发币的那条路径。
	cur, delta := int64(-1), int64(math.MinInt64)
	if raw := cur + delta; raw <= 0 {
		t.Fatalf("前提不成立：期望回绕成正数，实际 %d", raw)
	}
}
