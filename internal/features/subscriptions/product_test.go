package subscriptions

import "testing"

// 商品标识是三家渠道唯一能编码「买的是什么」的地方，解析错的后果是收了钱认不出商品。
func TestParseProductID(t *testing.T) {
	cases := []struct {
		name               string
		id, sep            string
		kind, tier, period string
		gb                 int // 0 = 应为 nil
		wantErr            bool
	}{
		// 三段式是基础订阅上线时的约定，必须继续认 —— 改约定不能让已在架的
		// SKU 一夜失效，那意味着所有存量订阅的续期事件同时开始报错。
		{"老三段式默认 plan", "squyrrl.basic.monthly", ".", "plan", "basic", "monthly", 0, false},
		{"四段式 plan", "squyrrl.plan.premium.yearly", ".", "plan", "premium", "yearly", 0, false},
		{"四段式 storage", "squyrrl.storage.s50.yearly", ".", "storage", "s50", "yearly", 50, false},
		{"Google 下划线分隔", "squyrrl_storage_s1024_yearly", "_", "storage", "s1024", "yearly", 1024, false},

		// 未上架的容量必须拒绝，而不是从 "s999" 里 parse 出 999 ——
		// 那样任何人在支付后台建一个商品就能凭空创造配额。
		{"未上架的容量", "squyrrl.storage.s999.yearly", ".", "", "", "", 0, true},
		{"未知 kind", "squyrrl.sticker.s50.yearly", ".", "", "", "", 0, true},
		{"段数不对", "squyrrl.basic", ".", "", "", "", 0, true},
		{"段数过多", "a.b.c.d.e", ".", "", "", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, tier, period, gb, err := ParseProductID(c.id, c.sep)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际得到 kind=%s tier=%s", kind, tier)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if kind != c.kind || tier != c.tier || period != c.period {
				t.Errorf("得到 %s/%s/%s，期望 %s/%s/%s", kind, tier, period, c.kind, c.tier, c.period)
			}
			if c.gb == 0 {
				if gb != nil {
					t.Errorf("plan 不该带容量，实际 %d", *gb)
				}
			} else if gb == nil || *gb != c.gb {
				t.Errorf("容量应为 %d，实际 %v", c.gb, gb)
			}
		})
	}
}
