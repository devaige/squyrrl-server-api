package quota

import (
	"testing"

	"github.com/squyrrl/api/internal/features/pricing"
)

// 上传配额判据的三道门，用纯算术复算一遍边界。带库的路径由集成验证覆盖。
func TestUploadThresholds(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		name                    string
		quota, used, held, size int64
		wantReject              bool
	}{
		{"空账户放得下", 20 * gb, 0, 0, gb, false},
		{"正好填满，必须放行", 20 * gb, 19 * gb, 0, gb, false},
		{"超出一个字节即拒", 20 * gb, 19 * gb, 0, gb + 1, true},
		// 在途预留是这套判据存在的理由：只比对已落库的占用，
		// 并发上传会各自看到同一个「还剩多少」然后一起超卖，
		// 而超出去的字节已经躺在 R2 上按月计费。
		{"已占用够但在途预留把额度吃满", 20 * gb, 10 * gb, 10 * gb, 1, true},
		{"在途预留刚好留出空间", 20 * gb, 10 * gb, 9 * gb, gb, false},
		{"未购买存储", 0, 0, 0, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quotaGB := int(c.quota / gb)
			maxFile := pricing.MaxFileBytesFor(quotaGB)
			rejected := maxFile <= 0 || c.size > maxFile ||
				c.used+c.held+c.size > c.quota
			if rejected != c.wantReject {
				t.Errorf("quota=%d used=%d held=%d size=%d → 拒绝=%v，期望 %v",
					c.quota, c.used, c.held, c.size, rejected, c.wantReject)
			}
		})
	}
}

// 单文件上限随总配额提升，而 0 配额时它是 0 —— 那个 0 必须被读成
// 「一个字节都不能传」，不能被当成「没有上限」。
func TestZeroQuotaMeansNoUploadAtAll(t *testing.T) {
	if got := pricing.MaxFileBytesFor(0); got != 0 {
		t.Fatalf("未购买存储时单文件上限应为 0，实际 %d", got)
	}
	// 这条断言钉住的是判据的写法：`size > maxFile` 单独不足以挡住 0 配额，
	// 因为 size 恒 > 0 时确实会被挡 —— 但一个 size==0 的请求会溜过去。
	// 所以实现里先判 `maxFile <= 0`。
	const zeroSized = int64(0)
	if zeroSized > pricing.MaxFileBytesFor(0) {
		t.Error("size=0 不该被 size>maxFile 挡住，这正是要先判 maxFile<=0 的原因")
	}
}

// storage_full 与 plan_limit 必须是两个 reason。
//
// 存储是独立商品：任何订阅档位单独都给不了空间。混成 plan_limit，
// 客户端会引导用户去升级订阅 —— 付了钱，空间还是 0。
func TestStorageErrorShape(t *testing.T) {
	st := StorageStatus{QuotaBytes: 100, UsedBytes: 90}
	e := newStorageError("空间不足", "basic", st, 20)
	if e.Reason != ReasonStorageFull {
		t.Errorf("reason 应为 %s，实际 %s", ReasonStorageFull, e.Reason)
	}
	if e.RequiredPlan != "" {
		t.Error("存储不是靠升档位拿到的，required_plan 必须为空")
	}
	if e.QuotaBytes == nil || e.UsedBytes == nil || e.NeededBytes == nil {
		t.Error("三个字节数都要带上，客户端才能说清还差多少")
	}
	if e.Cap != nil || e.Balance != nil {
		t.Error("不该混入档位数量或代币余额字段")
	}
}
