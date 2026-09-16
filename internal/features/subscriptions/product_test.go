package subscriptions

import "testing"

// 商品标识是三家渠道唯一能编码「买的是什么」的地方，解析错的后果是收了钱认不出商品。
func TestParseProductID(t *testing.T) {
	cases := []struct {
		name               string
		id, sep            string
		kind, tier, period string
		gb                 int   // 0 = 应为 nil
		credits            int64 // 0 = 不是代币商品
		wantErr            bool
	}{
		// 三段式是基础订阅上线时的约定，必须继续认 —— 改约定不能让已在架的
		// SKU 一夜失效，那意味着所有存量订阅的续期事件同时开始报错。
		{"老三段式默认 plan", "squyrrl.basic.monthly", ".", "plan", "basic", "monthly", 0, 0, false},
		{"四段式 plan", "squyrrl.plan.premium.yearly", ".", "plan", "premium", "yearly", 0, 0, false},
		{"四段式 storage", "squyrrl.storage.s40.monthly", ".", "storage", "s40", "monthly", 40, 0, false},
		{"Google 下划线分隔", "squyrrl_storage_s1280_monthly", "_", "storage", "s1280", "monthly", 1280, 0, false},

		// 代币加购此前整类解不出来（kind 只认 plan/storage）：在 App Store 里买
		// 一笔代币，事件收到后判未知档位丢弃，用户付了钱一个币都没有。
		{"四段式 credits", "squyrrl.credits.p5.once", ".", "credits", "p5", "once", 0, 55000, false},
		{"Google 的代币包", "squyrrl_credits_p50_once", "_", "credits", "p50", "once", 0, 625000, false},
		{"未上架的代币包", "squyrrl.credits.p7.once", ".", "", "", "", 0, 0, true},

		// 未上架的容量必须拒绝，而不是从 "s999" 里 parse 出 999 ——
		// 那样任何人在支付后台建一个商品就能凭空创造配额。
		{"未上架的容量", "squyrrl.storage.s999.monthly", ".", "", "", "", 0, 0, true},
		// 旧序列的档位：2026-09-16 换成 ×2 序列前卖过的键，现在必须拒收。
		{"已下架的容量", "squyrrl.storage.s1000.monthly", ".", "", "", "", 0, 0, true},
		{"未知 kind", "squyrrl.sticker.s40.monthly", ".", "", "", "", 0, 0, true},
		{"段数不对", "squyrrl.basic", ".", "", "", "", 0, 0, true},
		{"段数过多", "a.b.c.d.e", ".", "", "", "", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sku, err := ParseProductID(c.id, c.sep)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际得到 kind=%s tier=%s", sku.Kind, sku.Tier)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if sku.Kind != c.kind || sku.Tier != c.tier || sku.Period != c.period {
				t.Errorf("得到 %s/%s/%s，期望 %s/%s/%s",
					sku.Kind, sku.Tier, sku.Period, c.kind, c.tier, c.period)
			}
			if c.gb == 0 {
				if sku.StorageGB != nil {
					t.Errorf("非存储商品不该带容量，实际 %d", *sku.StorageGB)
				}
			} else if sku.StorageGB == nil || *sku.StorageGB != c.gb {
				t.Errorf("容量应为 %d，实际 %v", c.gb, sku.StorageGB)
			}
			if sku.Credits != c.credits {
				t.Errorf("代币数应为 %d，实际 %d", c.credits, sku.Credits)
			}
		})
	}
}
