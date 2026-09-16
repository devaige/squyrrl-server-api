package subscriptions

import (
	"strings"

	"github.com/squyrrl/api/internal/features/pricing"
)

// SKU 是从渠道商品标识里解出来的「这笔到底卖了什么」。
//
// 原先 ParseProductID 返回四个平铺的值（kind/tier/period/storageGB），加一个
// credits 就得再改一遍每个调用点的签名。换成结构体不是为了好看 —— 三家渠道的
// 商品维度只会继续长（配额、加量包、区域定价），平铺的返回值每长一维就是一次
// 全量改签名。
type SKU struct {
	Kind      string // plan | storage | credits
	Tier      string // 档位键 basic…maximum / 存储档 s20… / 代币包 p1…
	Period    string // monthly | yearly | once
	StorageGB *int   // kind=storage 时填
	Credits   int64  // kind=credits 时填，单次数量（未乘 quantity）
}

// ParseProductID 解析三家渠道的商品标识，得出这笔订阅卖的是什么。
//
// 约定：`squyrrl<sep><kind><sep><tier><sep><period>`，sep 由渠道决定
// （Apple 用 `.`，Google 用 `_`）。例如：
//
//	squyrrl.plan.basic.monthly
//	squyrrl.storage.s40.monthly
//	squyrrl.credits.p5.once
//
// **三段式向后兼容**：`squyrrl.basic.monthly` 仍解析为 plan。基础订阅是先上线的
// 商品，改约定不能让已经在架的 SKU 一夜失效 —— 那意味着所有存量订阅的续期
// 事件同时开始报错，而它们本来好好的。
//
// Stripe 不走这条路：它的商品由 metadata 描述（见 squyrrlMetadata），
// 因为 Stripe 的 price id 是随机串，没有可编码语义的地方。
func ParseProductID(id, sep string) (SKU, error) {
	parts := strings.Split(id, sep)
	var s SKU
	switch len(parts) {
	case 3:
		// 老约定：squyrrl<sep><tier><sep><period>
		return SKU{Kind: "plan", Tier: parts[1], Period: parts[2]}, nil
	case 4:
		s = SKU{Kind: parts[1], Tier: parts[2], Period: parts[3]}
	default:
		return SKU{}, ErrUnknownTier
	}

	switch s.Kind {
	case "plan":
		return s, nil
	case "storage":
		gb, ok := pricing.StorageGBForKey(s.Tier)
		if !ok {
			// 认不出的容量一律拒绝，而不是猜。放行的代价是凭空多出来的配额，
			// 且它会一直挂在那个用户账上直到有人手工发现。
			return SKU{}, ErrUnknownTier
		}
		s.StorageGB = &gb
		return s, nil
	case "credits":
		// 代币加购此前根本解不出来 —— kind 只认 plan/storage。表现是：在
		// App Store 里买一笔代币，webhook 收到后判 ErrUnknownTier 丢弃，
		// 用户付了钱、账上一个币没有，日志里只有一行「未知档位」。
		n, ok := pricing.CreditsForPackKey(s.Tier)
		if !ok {
			return SKU{}, ErrUnknownTier
		}
		s.Credits = n
		return s, nil
	default:
		return SKU{}, ErrUnknownTier
	}
}
