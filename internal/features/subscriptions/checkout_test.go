package subscriptions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/pricing"
)

func TestPriceBook(t *testing.T) {
	pb, err := NewPriceBook(`{"plan:basic:monthly":"price_a","storage:s50:yearly":"price_b"}`)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := pb.lookup("plan", "basic", "monthly"); !ok || id != "price_a" {
		t.Errorf("查不到 plan:basic:monthly，得到 %q %v", id, ok)
	}
	if _, ok := pb.lookup("plan", "premium", "monthly"); ok {
		t.Error("未配置的 SKU 不该查到")
	}
}

// 空配置是合法的（尚未接入支付），但购买入口必须是**关闭**的 ——
// 不能变成一个会在用户点下去之后才失败的按钮。
func TestEmptyPriceBookDisablesCheckout(t *testing.T) {
	pb, err := NewPriceBook("")
	if err != nil {
		t.Fatalf("空配置不该报错：%v", err)
	}
	if NewCheckout("sk_test_x", pb, "https://x").Enabled() {
		t.Error("没有价目表时购买入口应关闭")
	}
	// 只有价目表没有密钥同样不可用：否则用户会走到一个「商品不存在」的死胡同。
	full, _ := NewPriceBook(`{"plan:basic:monthly":"price_a"}`)
	if NewCheckout("", full, "https://x").Enabled() {
		t.Error("没有密钥时购买入口应关闭")
	}
	if !NewCheckout("sk_test_x", full, "https://x").Enabled() {
		t.Error("两项齐全时应可用")
	}
}

// 价目表填错不该让进程起不来，但要把错误报上去。
func TestBadPriceBookReturnsErrorNotPanic(t *testing.T) {
	pb, err := NewPriceBook(`{not json`)
	if err == nil {
		t.Error("非法 JSON 应报错")
	}
	if pb == nil {
		t.Fatal("即便解析失败也应返回一个可用的空价目表")
	}
	if NewCheckout("sk", pb, "https://x").Enabled() {
		t.Error("解析失败后购买入口应关闭")
	}
}

func TestCheckoutRejectsUnknownSKU(t *testing.T) {
	pb, _ := NewPriceBook(`{"plan:basic:monthly":"price_a"}`)
	c := NewCheckout("sk_test_x", pb, "https://squyrrl.com")
	// 没有网络调用：SKU 查不到就该在发请求之前返回。
	_, err := c.Create(context.Background(), uuid.New(), CheckoutRequest{
		Kind: "plan", Tier: "maximum", Period: "monthly",
	}, 0, 0)
	if !errors.Is(err, ErrUnknownSKU) {
		t.Errorf("未配置的 SKU 应判 ErrUnknownSKU，得到 %v", err)
	}
}

// 商品标识与 pricing 的目录必须对得上 —— 两边各写一套键，
// 表现是「后台配了价格但服务端说商品不存在」。
func TestSKUKeysMatchCatalog(t *testing.T) {
	if got := pricing.StorageTierKey(50); got != "s50" {
		t.Errorf("存储档位键应为 s50，实际 %s", got)
	}
	if gb, ok := pricing.StorageGBForKey("s50"); !ok || gb != 50 {
		t.Errorf("s50 应反查出 50 GB，得到 %d %v", gb, ok)
	}
	if got := pricing.CreditPackKey(5); got != "p5" {
		t.Errorf("代币档位键应为 p5，实际 %s", got)
	}
	if n, ok := pricing.CreditsForPackKey("p5"); !ok || n != 55_000 {
		t.Errorf("p5 应反查出 55000 代币，得到 %d %v", n, ok)
	}
	// 未上架的键一律查不到，否则支付后台建个商品就能凭空创造额度。
	if _, ok := pricing.StorageGBForKey("s999"); ok {
		t.Error("未上架的存储档位不该查得到")
	}
	if _, ok := pricing.CreditsForPackKey("p9999"); ok {
		t.Error("未上架的代币档位不该查得到")
	}
}
