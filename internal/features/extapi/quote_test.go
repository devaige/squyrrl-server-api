package extapi

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/squyrrl/api/internal/features/pricing"
)

// ep 造一个最小可用的 endpoint：有驱动配置、有余额。
func ep(slug string, unitPriceMicros int64, marginBP int32) Endpoint {
	return Endpoint{
		Slug:            slug,
		Enabled:         true,
		UnitPriceMicros: unitPriceMicros,
		MarginBP:        marginBP,
		BalanceMicros:   1_000_000,
		Config:          json.RawMessage(`{"url_template":"https://x/${resource_id}"}`),
	}
}

// 报价取候选链上的最高价，而不是 priority 最靠前的那个。
//
// 这条是整批改动的核心断言：兜底链降级到更贵的 endpoint 时，收费不能低于支出。
func TestQuoteTakesMostExpensiveCandidate(t *testing.T) {
	eps := []Endpoint{
		ep("cheap", 1_000, pricing.DefaultMarginBP),  // $0.001 × 2 = 20 credits
		ep("pricey", 5_000, pricing.DefaultMarginBP), // $0.005 × 2 = 100 credits
	}
	got, err := quoteOf(eps)
	if err != nil {
		t.Fatalf("quoteOf: %v", err)
	}
	want, _ := pricing.CreditCost(5_000, pricing.DefaultMarginBP)
	if got != want {
		t.Fatalf("报价 %d，期望链上最高价 %d", got, want)
	}
}

// 不可用的 endpoint 既不参与调用，也不参与报价 —— 两侧必须同进同出。
func TestQuoteSkipsUnusableSameAsFetch(t *testing.T) {
	noDriver := ep("no-driver", 9_000, pricing.DefaultMarginBP)
	noDriver.Config = json.RawMessage(`{}`)

	drained := ep("drained", 9_000, pricing.DefaultMarginBP)
	drained.BalanceMicros = 0

	badMargin := ep("bad-margin", 9_000, 0) // CHECK 之外的历史行 / 直接改表

	badCfg := ep("bad-cfg", 9_000, pricing.DefaultMarginBP)
	badCfg.Config = json.RawMessage(`{not json`)

	for _, c := range []struct {
		name string
		e    Endpoint
		want skipReason
	}{
		{"未配驱动", noDriver, skipNoDriver},
		{"余额耗尽", drained, skipNoBalance},
		{"倍率非法", badMargin, skipNoPricing},
		{"配置坏了", badCfg, skipBadConfig},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, skip := usable(c.e); skip != c.want {
				t.Fatalf("usable skip=%q，期望 %q", skip, c.want)
			}
			if _, err := quoteOf([]Endpoint{c.e}); !errors.Is(err, ErrNoEndpoint) {
				t.Fatalf("单个不可用 endpoint 应报 ErrNoEndpoint，得到 %v", err)
			}
		})
	}
}

// 一条链里混着可用与不可用时，报价只看可用的那些。
func TestQuoteIgnoresUnusableInMixedChain(t *testing.T) {
	drained := ep("drained-expensive", 50_000, pricing.DefaultMarginBP)
	drained.BalanceMicros = 0

	got, err := quoteOf([]Endpoint{drained, ep("live", 2_000, pricing.DefaultMarginBP)})
	if err != nil {
		t.Fatalf("quoteOf: %v", err)
	}
	want, _ := pricing.CreditCost(2_000, pricing.DefaultMarginBP)
	if got != want {
		t.Fatalf("报价 %d，期望 %d —— 余额耗尽的贵 endpoint 不该抬高报价", got, want)
	}
}

// 没有任何 endpoint 时返回 ErrNoEndpoint，调用方据此退回内置免费实现的价。
func TestQuoteEmptyChain(t *testing.T) {
	if _, err := quoteOf(nil); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("空链应报 ErrNoEndpoint，得到 %v", err)
	}
}

// 上游免费（unit_price=0）但已接好驱动的 endpoint 仍要收下限价，不能免单。
func TestQuoteZeroUpstreamStillChargesFloor(t *testing.T) {
	got, err := quoteOf([]Endpoint{ep("free-upstream", 0, pricing.DefaultMarginBP)})
	if err != nil {
		t.Fatalf("quoteOf: %v", err)
	}
	if got != pricing.MinCreditCost {
		t.Fatalf("报价 %d，期望下限 %d", got, pricing.MinCreditCost)
	}
}
