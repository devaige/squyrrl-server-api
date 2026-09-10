package quota

import (
	"testing"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// minTierFor 是这个包里唯一决定「升级按钮指向哪一档」的逻辑，
// 指错档位的表现是用户付了钱仍然做不了那件事。
func TestMinTierFor(t *testing.T) {
	cases := []struct {
		limit Limit
		need  int
		want  string
	}{
		// free 的碎片上限是 1000，第 1001 条需要 basic
		{LimitSnippets, 1_000, entitlement.Free},
		{LimitSnippets, 1_001, entitlement.Basic},
		{LimitSnippets, 10_001, entitlement.Standard},
		{LimitSnippets, 100_001, entitlement.Premium},
		{LimitSnippets, 1_000_001, entitlement.Maximum},

		{LimitPages, 8, entitlement.Free},
		{LimitPages, 9, entitlement.Basic},
		{LimitPages, 129, ""}, // 超过最高档

		{LimitTags, 17, entitlement.Basic},
		{LimitDevices, 1, entitlement.Basic}, // free 的 Devices 是 0（不适用）
		{LimitBindings, 1, entitlement.Basic},
		{LimitBindings, 9, ""}, // maximum 是 8
	}
	for _, c := range cases {
		if got := minTierFor(c.limit, c.need); got != c.want {
			t.Errorf("minTierFor(%s, %d) = %q，期望 %q", c.limit, c.need, got, c.want)
		}
	}
}

// 「最高档也不够」必须返回空串而不是 maximum —— 否则至尊版用户会看到
// 一个指向自己当前档位的升级按钮，点了毫无变化。
func TestBeyondTopTierReturnsEmpty(t *testing.T) {
	top, _ := entitlement.Of(entitlement.Maximum)
	if got := minTierFor(LimitSnippets, top.Snippets+1); got != "" {
		t.Errorf("超出最高档应返回空串，实际 %q", got)
	}
	// 正好等于上限时仍应指向最高档
	if got := minTierFor(LimitSnippets, top.Snippets); got != entitlement.Maximum {
		t.Errorf("正好用满最高档应返回 maximum，实际 %q", got)
	}
}

func TestFirstTierWithCapability(t *testing.T) {
	if got := firstTierWith(func(x entitlement.Tier) bool { return x.HiddenPages }); got != entitlement.Standard {
		t.Errorf("隐藏页面应从 standard 起，实际 %q", got)
	}
	if got := firstTierWith(func(x entitlement.Tier) bool { return x.Sync }); got != entitlement.Basic {
		t.Errorf("同步应从 basic 起，实际 %q", got)
	}
	if got := firstTierWith(func(x entitlement.Tier) bool { return x.ParsePriority }); got != entitlement.Maximum {
		t.Errorf("优先解析应从 maximum 起，实际 %q", got)
	}
	// 无档位满足时返回空串
	if got := firstTierWith(func(x entitlement.Tier) bool { return false }); got != "" {
		t.Errorf("无档位满足应返回空串，实际 %q", got)
	}
}

// 三种 402 的响应体不能混填字段：客户端看到 balance 和 cap 同时存在会不知道该读哪个。
func TestErrorShapesAreDisjoint(t *testing.T) {
	credits := ErrInsufficientCredits(120, 200)
	if credits.Reason != ReasonInsufficientCredits {
		t.Errorf("reason 应为 %s", ReasonInsufficientCredits)
	}
	if credits.Cap != nil || credits.Current != nil || credits.Limit != "" {
		t.Error("余额不足的响应里不该出现档位字段")
	}
	if credits.Balance == nil || credits.Required == nil {
		t.Error("余额不足必须带 balance 与 required")
	}

	limit := newLimitError(LimitPages, entitlement.Basic, 16, 16)
	if limit.Reason != ReasonPlanLimit {
		t.Errorf("reason 应为 %s", ReasonPlanLimit)
	}
	if limit.Balance != nil || limit.Required != nil {
		t.Error("档位超限的响应里不该出现余额字段")
	}
	if limit.RequiredPlan != entitlement.Standard {
		t.Errorf("页面 17 个应指向 standard，实际 %q", limit.RequiredPlan)
	}

	sync := ErrSyncRequired(entitlement.Free)
	if sync.Reason != ReasonSyncRequired {
		t.Errorf("reason 应为 %s", ReasonSyncRequired)
	}
	if sync.RequiredPlan != entitlement.Basic {
		t.Errorf("同步应指向 basic，实际 %q", sync.RequiredPlan)
	}
}

// Error 必须能被 errors 体系识别成 error，且 handler 能取回结构体。
func TestIsQuotaError(t *testing.T) {
	var err error = ErrSyncRequired(entitlement.Free)
	qe, ok := IsQuotaError(err)
	if !ok {
		t.Fatal("应能识别为 quota.Error")
	}
	if qe.Reason != ReasonSyncRequired {
		t.Errorf("取回的 reason 不对：%s", qe.Reason)
	}
	if err.Error() == "" {
		t.Error("Error() 不应为空")
	}
}

// capOf 覆盖所有数量型 Limit —— 漏一个的表现是该门槛上限恒为 0，
// 于是用户第一次操作就被拒，而不是到了上限才被拒。
func TestCapOfCoversAllCountLimits(t *testing.T) {
	top, _ := entitlement.Of(entitlement.Maximum)
	for _, l := range []Limit{LimitSnippets, LimitPages, LimitTags, LimitDevices, LimitBindings} {
		if capOf(top, l) <= 0 {
			t.Errorf("Limit %q 在 capOf 里没有对应分支（返回 %d）", l, capOf(top, l))
		}
	}
}
