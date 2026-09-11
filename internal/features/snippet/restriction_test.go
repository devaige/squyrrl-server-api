package snippet

import (
	"testing"
	"time"

	"github.com/squyrrl/api/internal/features/entitlement"
)

func TestRestrictionStages(t *testing.T) {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want Restriction
	}{
		{"刚标记", base, RestrictionInGrace},
		{"宽限期最后一刻", base.Add(RestrictionGrace - time.Second), RestrictionInGrace},
		{"宽限期满即转冻结", base.Add(RestrictionGrace), RestrictionFrozen},
		{"冻结期内", base.Add(RestrictionGrace + 15*24*time.Hour), RestrictionFrozen},
		{"冻结期满", base.Add(RestrictionGrace + RestrictionFreeze), RestrictionFrozen},
		// 已过冻结期但清理任务还没跑到时，**绝不能放开**。若这里回落成
		// RestrictionNone，一个停了几天的清理任务会让本该被删的数据重新变得可读可改。
		{"过了冻结期但尚未清理", base.Add(200 * 24 * time.Hour), RestrictionFrozen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := base
			if got := RestrictionOf(&at, c.at); got != c.want {
				t.Errorf("在 %v 应为 %q，实际 %q", c.at.Sub(base), c.want, got)
			}
		})
	}

	if got := RestrictionOf(nil, base); got != RestrictionNone {
		t.Errorf("未标记的碎片应为正常，实际 %q", got)
	}
}

// 阶段结束时刻要能直接拿去做倒计时 —— 客户端显示「还有 N 天」靠的就是它。
func TestStageEnd(t *testing.T) {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if got := StageEnd(base, RestrictionInGrace); !got.Equal(base.Add(30 * 24 * time.Hour)) {
		t.Errorf("宽限期应在 30 天后结束，实际 %v", got)
	}
	if got := StageEnd(base, RestrictionFrozen); !got.Equal(base.Add(60 * 24 * time.Hour)) {
		t.Errorf("冻结期应在 60 天后结束，实际 %v", got)
	}
}

// 服务端额度与本地额度是两回事，这一条是整个降级链路的判据来源。
//
// 读错它的后果很具体：拿 Snippets 当服务端上限，降到免费档的用户会被判定为
// 「还能在云上留 1000 条」—— 而那 1000 行是真实的服务端成本，
// 恰恰是「免费档零成本」这个前提要排除的东西。
func TestServerSnippetsIsZeroForFree(t *testing.T) {
	free, _ := entitlement.Of(entitlement.Free)
	if free.Snippets != 1000 {
		t.Fatalf("前提变了：免费档本地额度现在是 %d", free.Snippets)
	}
	if got := free.ServerSnippets(); got != 0 {
		t.Errorf("免费档的服务端额度应为 0，实际 %d", got)
	}
	for _, key := range []string{entitlement.Basic, entitlement.Standard, entitlement.Premium, entitlement.Maximum} {
		tier, _ := entitlement.Of(key)
		if tier.ServerSnippets() != tier.Snippets {
			t.Errorf("%s 的服务端额度应等于本地额度", key)
		}
	}
}

// 两个阶段各 30 天，合计 60 天 —— purge 的判据用的是这个和。
func TestLifecycleTotalIsSixtyDays(t *testing.T) {
	if RestrictionGrace+RestrictionFreeze != 60*24*time.Hour {
		t.Errorf("生命周期总长应为 60 天，实际 %v", RestrictionGrace+RestrictionFreeze)
	}
}
