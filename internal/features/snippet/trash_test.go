package snippet

import (
	"testing"

	"github.com/squyrrl/api/internal/features/entitlement"
)

// minRetentionDays 是粗筛阈值：Sweep 先用它挑出「可能有过期回收站条目」的用户，
// 再逐个按真实档位裁定。它**必须小于等于任何一个启用了回收站的档位**，
// 否则粗筛会漏掉本该清理的用户 —— 而漏掉的表现是回收站永远不清，
// 一个不报错、只会让库慢慢变大的 bug。
func TestMinRetentionIsFloorOfAllEnabledTiers(t *testing.T) {
	min := minRetentionDays()
	if min <= 0 {
		t.Fatal("至少应有一个档位启用回收站")
	}
	for _, key := range entitlement.Order {
		tier, ok := entitlement.Of(key)
		if !ok || tier.TrashDays <= 0 {
			continue
		}
		if tier.TrashDays < min {
			t.Errorf("档位 %s 的保留期 %d 天小于粗筛阈值 %d 天，该档用户永远不会被清理",
				key, tier.TrashDays, min)
		}
	}
}

// 免费档的 TrashDays 是 0，而 0 在这里的含义是「不启用回收站」，
// 绝不能被读成「立即删除」。会落到这条路径上的只有降级用户，
// 对他们来说「订阅到期」与「永久销毁已删内容」之间不该只隔一次定时任务。
func TestFreeTierIsExcludedFromSweep(t *testing.T) {
	free, _ := entitlement.Of(entitlement.Free)
	if free.TrashDays != 0 {
		t.Fatalf("前提变了：免费档的 TrashDays 现在是 %d", free.TrashDays)
	}
	if minRetentionDays() == 0 {
		t.Error("粗筛阈值不应把 0 当成一个有效保留期")
	}
}

// 保留期随档位单调不减 —— 升级之后回收站变短会是个荒谬的退化。
func TestTrashDaysMonotonic(t *testing.T) {
	prev := -1
	for _, key := range entitlement.Order {
		tier, _ := entitlement.Of(key)
		if tier.TrashDays < prev {
			t.Errorf("档位 %s 的保留期 %d 天比前一档的 %d 天还短", key, tier.TrashDays, prev)
		}
		prev = tier.TrashDays
	}
}
