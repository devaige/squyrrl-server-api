package wallet

import (
	"testing"
	"time"

	"github.com/squyrrl/api/internal/features/pricing"
)

// 宽限期的长度是一个刻意的取舍，值得钉住：ADR-075 ⑧ 之后「掉回 free」
// 在客户端意味着分流切回本地 —— 用户的云端碎片当场从视野消失，
// 新写入静默落到本机盘。而绝大多数订阅中断是扣款失败而非主动取消。
// 把它调短省下的是几天服务成本，赔上的是一次看起来像数据丢失的体验。
func TestGracePeriodIsTwoWeeks(t *testing.T) {
	if GracePeriod != 14*24*time.Hour {
		t.Errorf("宽限期应为 14 天，实际 %v", GracePeriod)
	}
	if GracePeriod != pricing.GracePeriod {
		t.Error("wallet 的别名与 pricing 的定义不一致")
	}
	// 短于一个自然计费周期才有意义；等于或长于一个月会让「过期」永远到不了。
	if GracePeriod >= 28*24*time.Hour {
		t.Error("宽限期不应接近一个计费周期")
	}
}

// 三个状态串必须与 SQL 里写的字面量一致 —— PlanState 的查询按 status 分支，
// 常量与查询分叉的表现是「付费用户被当成没订阅」。
func TestPlanStatusConstantsMatchSchema(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{PlanStatusNone, "none"},
		{PlanStatusActive, "active"},
		{PlanStatusPastDue, "past_due"},
	} {
		if c.got != c.want {
			t.Errorf("状态常量应为 %q，实际 %q", c.want, c.got)
		}
	}
}
