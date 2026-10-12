package wallet

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeAdminReason(t *testing.T) {
	ok := []struct{ in, want string }{
		{"admin_refund:工单1234 重复扣费退回", "admin_refund:工单1234 重复扣费退回"},
		{"  admin_comp: 10-10 解析故障补偿  ", "admin_comp:10-10 解析故障补偿"},
		// 说明里再出现冒号是正常的（时间、URL），只按第一个冒号切。
		{"admin_grant:活动 2026-10-01 10:00 场", "admin_grant:活动 2026-10-01 10:00 场"},
		{"admin_test:" + strings.Repeat("测", MaxAdminReasonDetail), "admin_test:" + strings.Repeat("测", MaxAdminReasonDetail)},
	}
	for _, c := range ok {
		got, err := normalizeAdminReason(c.in)
		if err != nil {
			t.Errorf("%q 应通过，得到错误：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q 规范化为 %q，期望 %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"",
		"admin_grant",          // 旧的默认值：没有说明
		"admin_grant:",         // 说明为空
		"admin_grant:   ",      // 说明只有空白
		"补偿:故障",                // 类别不在白名单
		"ADMIN_GRANT:大小写不同",    // 类别必须原样匹配
		"purchase:stripe",      // 系统 reason 不能从后台冒充
		"admin_grant :多了空格",    // 类别里混入空白
		"admin_grant:第一行\n第二行", // 换行会让导出的对账表错位
		"admin_grant:" + strings.Repeat("a", MaxAdminReasonDetail+1),
	}
	for _, in := range bad {
		if _, err := normalizeAdminReason(in); !errors.Is(err, ErrInvalidAdminReason) {
			t.Errorf("%q 应以 ErrInvalidAdminReason 拒绝，得到 %v", in, err)
		}
	}
}

// 类别会被后台下拉框照抄、被对账 SQL 按前缀筛选，必须都以 admin_ 开头且不含冒号。
func TestAdminReasonCategoriesShape(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range AdminReasonCategories {
		if !strings.HasPrefix(c, "admin_") || strings.Contains(c, ":") {
			t.Errorf("类别 %q 不符合 admin_<name> 形式", c)
		}
		if seen[c] {
			t.Errorf("类别 %q 重复", c)
		}
		seen[c] = true
	}
}
