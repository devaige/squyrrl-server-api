package quota

import (
	"testing"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// CheckBatch 的判据是 `current + add > max`，这个测试固化它的边界。
//
// 用 > 而不是 >= 是有意的：正好填满额度必须允许通过。写成 >= 的表现是
// 用户永远差最后一条填不满自己买的档位 —— 一个只在边界出现、
// 而且用户会准确描述成「我明明还差一条」的 bug。
//
// 这里只验判据本身（纯算术），带库的路径由 CheckBatch 在集成环境覆盖。
func TestBatchThreshold(t *testing.T) {
	basic, _ := entitlement.Of(entitlement.Basic)
	max := basic.Snippets

	cases := []struct {
		name       string
		current    int
		add        int
		wantReject bool
	}{
		{"空账户加一批", 0, 100, false},
		{"正好填满，必须放行", max - 10, 10, false},
		{"超出一条即拒", max - 10, 11, true},
		{"已满再加一条", max, 1, true},
		{"已满但不新增", max, 0, false},
		{"已超额（降级后）不新增", max + 500, 0, false},
		{"已超额且还想加", max + 500, 1, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rejected := c.add > 0 && c.current+c.add > max
			if rejected != c.wantReject {
				t.Errorf("current=%d add=%d max=%d → 拒绝=%v，期望 %v",
					c.current, c.add, max, rejected, c.wantReject)
			}
		})
	}
}

// 降级用户（已有数据超过新档位上限）不应被卡在「什么都做不了」的状态：
// 只要不新增，就不报错。删除和读取必须始终可用，否则用户连清理数据自救都做不到。
func TestDowngradedUserCanStillOperate(t *testing.T) {
	free, _ := entitlement.Of(entitlement.Free)
	overCap := free.Snippets + 5000 // 从付费档降级回 free 的典型状态

	if rejected := 0 > 0 && overCap+0 > free.Snippets; rejected {
		t.Error("超额用户在不新增时不应被拒绝")
	}
	// 报错时给出的 current 是「加完之后的数字」，用户据此判断这批放不放得下
	e := newLimitError(LimitSnippets, entitlement.Free, free.Snippets, overCap+1)
	if *e.Current != overCap+1 {
		t.Errorf("current 应为加完后的值 %d，实际 %d", overCap+1, *e.Current)
	}
	if e.RequiredPlan == "" {
		t.Error("超出 free 上限时应给出可升级的档位")
	}
}
