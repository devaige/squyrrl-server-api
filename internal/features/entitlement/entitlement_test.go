package entitlement

import (
	"reflect"
	"strings"
	"testing"
)

// 五个档位键是三家支付渠道商品 ID 约定的一部分，少一个就意味着某档的 webhook 会被拒。
func TestAllTiersPresent(t *testing.T) {
	if len(table) != len(Order) {
		t.Fatalf("表里有 %d 档，Order 列了 %d 档", len(table), len(Order))
	}
	for _, k := range Order {
		tier, ok := Of(k)
		if !ok {
			t.Fatalf("档位 %q 不在表里", k)
		}
		if tier.Key != k {
			t.Errorf("档位 %q 的 Key 字段是 %q，应与键一致", k, tier.Key)
		}
	}
}

// 这是本包唯一一个真正防事故的测试：数值梯度必须单调不降。
// 手改一个门槛时把 premium 写得比 standard 还低，代码照样编译、接口照样返回，
// 表现是「升级之后功能变少」——只有断言能抓住。
func TestLaddersAreMonotonic(t *testing.T) {
	nums := map[string]func(Tier) int{
		"Snippets":            func(x Tier) int { return x.Snippets },
		"Pages":               func(x Tier) int { return x.Pages },
		"Tags":                func(x Tier) int { return x.Tags },
		"Devices":             func(x Tier) int { return x.Devices },
		"BindingsPerPlatform": func(x Tier) int { return x.BindingsPerPlatform },
		"TrashDays":           func(x Tier) int { return x.TrashDays },
		"PriceUSDMonthly":     func(x Tier) int { return x.PriceUSDMonthly },
	}
	for name, get := range nums {
		for i := 1; i < len(Order); i++ {
			lo, _ := Of(Order[i-1])
			hi, _ := Of(Order[i])
			if get(hi) < get(lo) {
				t.Errorf("%s 在 %s→%s 处下降：%d → %d",
					name, Order[i-1], Order[i], get(lo), get(hi))
			}
		}
	}

	// 布尔能力同理：一旦某档开启，更高档不得关闭。
	bools := map[string]func(Tier) bool{
		"Sync":          func(x Tier) bool { return x.Sync },
		"HiddenPages":   func(x Tier) bool { return x.HiddenPages },
		"Rules":         func(x Tier) bool { return x.Rules },
		"Import":        func(x Tier) bool { return x.Import },
		"ParsePriority": func(x Tier) bool { return x.ParsePriority },
	}
	for name, get := range bools {
		for i := 1; i < len(Order); i++ {
			lo, _ := Of(Order[i-1])
			hi, _ := Of(Order[i])
			if get(lo) && !get(hi) {
				t.Errorf("%s 在 %s→%s 处被关掉了", name, Order[i-1], Order[i])
			}
		}
	}
}

// ADR-075 的硬约束：基础订阅不含 credits、不含存储，所以 Tier 不该出现任何相关字段。
//
// 用反射遍历真实字段而不是比对一份手写清单 —— 后者在「有人新增了字段却忘了同步清单」
// 时恰好失效，而那正是这条断言唯一要防的场景。
func TestNoBundledCreditsOrStorage(t *testing.T) {
	forbidden := []string{"Credit", "Storage", "GB", "Bytes", "Quota"}
	typ := reflect.TypeOf(Tier{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("字段 %q 看起来在往基础订阅里塞 credits 或存储；"+
					"ADR-075 要求这两样是独立商品，各有自己的计费路径", name)
			}
		}
	}
}

// free 必须是纯本地档：Sync 关闭，且不占用任何服务端配额概念。
func TestFreeIsLocalOnly(t *testing.T) {
	f, _ := Of(Free)
	if f.Sync {
		t.Error("free 档不应同步——它开着同步就产生了服务端成本")
	}
	if f.Devices != 0 {
		t.Errorf("free 的 Devices 应为 0（不适用），实际 %d", f.Devices)
	}
	if f.TrashDays != 0 {
		t.Errorf("free 的 TrashDays 应为 0（回收站也在本地），实际 %d", f.TrashDays)
	}
	if f.PriceUSDMonthly != 0 {
		t.Errorf("free 应免费，实际 $%d", f.PriceUSDMonthly)
	}
}

func TestKnownAndAtLeast(t *testing.T) {
	if Known("gold") {
		t.Error("不存在的档位不该被认为合法")
	}
	if !Known(Maximum) {
		t.Error("maximum 应合法")
	}
	if !AtLeast(Premium, Standard) {
		t.Error("premium 应达到 standard")
	}
	if AtLeast(Basic, Premium) {
		t.Error("basic 不应达到 premium")
	}
	if AtLeast("gold", Free) {
		t.Error("未知档位必须 fail-closed")
	}
	if MustOf("gold").Key != Free {
		t.Error("MustOf 对未知档位应回落 free")
	}
}
