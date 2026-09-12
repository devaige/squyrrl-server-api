package subscriptions

import (
	"strings"

	"github.com/squyrrl/api/internal/features/pricing"
)

// ParseProductID 解析三家渠道的商品标识，得出这笔订阅卖的是什么。
//
// 约定：`squyrrl<sep><kind><sep><tier><sep><period>`，sep 由渠道决定
// （Apple 用 `.`，Google 用 `_`）。例如：
//
//	squyrrl.plan.basic.monthly
//	squyrrl.storage.s50.yearly
//
// **三段式向后兼容**：`squyrrl.basic.monthly` 仍解析为 plan。基础订阅是先上线的
// 商品，改约定不能让已经在架的 SKU 一夜失效 —— 那意味着所有存量订阅的续期
// 事件同时开始报错，而它们本来好好的。
//
// Stripe 不走这条路：它的商品由 metadata 描述（见 squyrrlMetadata），
// 因为 Stripe 的 price id 是随机串，没有可编码语义的地方。
func ParseProductID(id, sep string) (kind, tier, period string, storageGB *int, err error) {
	parts := strings.Split(id, sep)
	switch len(parts) {
	case 3:
		// 老约定：squyrrl<sep><tier><sep><period>
		return "plan", parts[1], parts[2], nil, nil
	case 4:
		kind, tier, period = parts[1], parts[2], parts[3]
	default:
		return "", "", "", nil, ErrUnknownTier
	}

	switch kind {
	case "plan":
		return kind, tier, period, nil, nil
	case "storage":
		gb, ok := pricing.StorageGBForKey(tier)
		if !ok {
			// 认不出的容量一律拒绝，而不是猜。放行的代价是凭空多出来的配额，
			// 且它会一直挂在那个用户账上直到有人手工发现。
			return "", "", "", nil, ErrUnknownTier
		}
		return kind, tier, period, &gb, nil
	default:
		return "", "", "", nil, ErrUnknownTier
	}
}
