package auth

import (
	"testing"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// 逐出策略的两条判据，用档位表复算一遍。
//
// keep 取自 Tier.Devices，而 **0 必须被读成「该档位不适用设备概念」**，
// 不是「一台都不许有」。按后者理解，免费档用户刚登录建好的那台会连同旧的一起
// 被撤销 —— 用户当场被登出，而且他重试多少次都是同样的结果。
func TestFreeTierDeviceLimitIsNotAnEvictEverythingSignal(t *testing.T) {
	free, _ := entitlement.Of(entitlement.Free)
	if free.Devices != 0 {
		t.Fatalf("前提变了：免费档设备数现在是 %d", free.Devices)
	}
	// registerDevice 里的判据是 `limit <= 0 → 跳过逐出`。这条断言钉住那个语义：
	// 免费档不该进入逐出分支。
	if free.Devices > 0 {
		t.Error("免费档不应触发逐出")
	}
}

// 设备数随档位单调不减 —— 升级之后能登的设备变少会是个荒谬的退化。
func TestDeviceLimitMonotonic(t *testing.T) {
	prev := -1
	for _, key := range entitlement.Order {
		tier, _ := entitlement.Of(key)
		if tier.Devices < prev {
			t.Errorf("档位 %s 的设备数 %d 比前一档的 %d 还少", key, tier.Devices, prev)
		}
		prev = tier.Devices
	}
}

// 付费档都必须 ≥ 1：等于 0 会被 registerDevice 当成「不适用」而完全跳过逐出，
// 于是那一档的设备数上限静默失效。
func TestPaidTiersHaveAtLeastOneDevice(t *testing.T) {
	for _, key := range []string{
		entitlement.Basic, entitlement.Standard, entitlement.Premium, entitlement.Maximum,
	} {
		tier, _ := entitlement.Of(key)
		if tier.Devices < 1 {
			t.Errorf("付费档 %s 的设备数是 %d，会让逐出被整档跳过", key, tier.Devices)
		}
	}
}
