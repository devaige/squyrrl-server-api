package config

import (
	"testing"

	"github.com/squyrrl/api/internal/features/pricing"
)

// 直传的 fail-closed 语义靠这几条守住：生产漏配不该「能跑但字节穿过服务器」，
// 也不该「能跑但上传静默 503」，而应当直接拒绝启动。
func TestValidateProdUploadConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"dev 允许全默认", Config{Env: "dev", UploadTokenSecret: devUploadTokenSecret}, false},
		{"dev 允许空 edge base", Config{Env: "dev"}, false},
		{"prod 缺 edge base", Config{Env: "prod", UploadTokenSecret: "real-secret"}, true},
		{"prod 用 dev 密钥", Config{Env: "prod", UploadEdgeBase: "https://files.squyrrl.com", UploadTokenSecret: devUploadTokenSecret}, true},
		{"prod 缺密钥", Config{Env: "prod", UploadEdgeBase: "https://files.squyrrl.com"}, true},
		{"prod 齐备", Config{Env: "prod", UploadEdgeBase: "https://files.squyrrl.com", UploadTokenSecret: "real-secret"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// envDefault 只能写字面量，与 devUploadTokenSecret 常量之间没有编译期约束，
// 靠这条断言防止两处漂移后 Validate 再也拦不住 dev 密钥进生产。
func TestDevTokenSecretMatchesEnvDefault(t *testing.T) {
	t.Setenv("SQUYRRL_DATABASE_URL", "postgres://localhost/x")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UploadTokenSecret != devUploadTokenSecret {
		t.Fatalf("envDefault=%q 与 devUploadTokenSecret=%q 不一致", cfg.UploadTokenSecret, devUploadTokenSecret)
	}
}

// 内置免费 provider 的解析价必须等于 pricing.MinCreditCost。
//
// 同样是 envDefault 写不了常量引用的老问题，但这一条的漂移后果更隐蔽：
// 付费 endpoint 的价格由 CreditCost 兜着下限，内置 provider 的价格却只有这一个
// 数字说了算。它被调低时没有任何东西会红，表现只是每次免费解析少收一点 ——
// 而免费 provider 恰恰是调用量最大的那批。
func TestParseCostDefaultMatchesMinCreditCost(t *testing.T) {
	t.Setenv("SQUYRRL_DATABASE_URL", "postgres://localhost/x")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ParseCost != pricing.MinCreditCost {
		t.Fatalf("SQUYRRL_PARSE_COST 默认值 %d 与 pricing.MinCreditCost=%d 不一致",
			cfg.ParseCost, pricing.MinCreditCost)
	}
}
