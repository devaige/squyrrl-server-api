package config

import (
	"testing"

	"github.com/squyrrl/api/internal/features/pricing"
)

// 直传的 fail-closed 语义靠这几条守住：生产漏配不该「能跑但字节穿过服务器」，
// 也不该「能跑但上传静默 503」，而应当直接拒绝启动。
func TestValidateUploadConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"全默认", Config{}, true},
		{"缺 edge base", Config{UploadTokenSecret: "real-secret"}, true},
		{"使用已废弃的开发密钥", Config{UploadEdgeBase: "https://files.squyrrl.com", UploadTokenSecret: "dev-upload-token-secret"}, true},
		{"缺密钥", Config{UploadEdgeBase: "https://files.squyrrl.com"}, true},
		{"齐备", Config{UploadEdgeBase: "https://files.squyrrl.com", UploadTokenSecret: "real-secret"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// 旧的环境变量不能再绕过正式配置校验，签票密钥也不能隐式使用开发值。
func TestLegacyDevEnvironmentDoesNotBypassValidation(t *testing.T) {
	t.Setenv("SQUYRRL_DATABASE_URL", "postgres://localhost/x")
	t.Setenv("SQUYRRL_ENV", "dev")
	t.Setenv("SQUYRRL_UPLOAD_EDGE_BASE", "")
	t.Setenv("SQUYRRL_UPLOAD_TOKEN_SECRET", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UploadTokenSecret != "" {
		t.Fatal("签票密钥不应有默认值")
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("旧 SQUYRRL_ENV=dev 不应绕过配置校验")
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
