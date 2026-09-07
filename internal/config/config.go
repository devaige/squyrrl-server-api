package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Env      string `env:"SQUYRRL_ENV"      envDefault:"dev"`
	HTTPAddr string `env:"SQUYRRL_HTTP_ADDR" envDefault:":8080"`

	DatabaseURL string `env:"SQUYRRL_DATABASE_URL,required"`

	S3Endpoint string `env:"SQUYRRL_S3_ENDPOINT"`
	// bucket 名跨环境固定为 squyrrl：设成默认值而非 required，好把这一行从所有 env 模板里彻底
	// 移除（dev MinIO 启动自动建桶，prod R2 预建同名桶即可）。真要改仍可用环境变量覆盖。
	S3Bucket    string `env:"SQUYRRL_S3_BUCKET" envDefault:"squyrrl"`
	S3AccessKey string `env:"SQUYRRL_S3_ACCESS_KEY"`
	S3SecretKey string `env:"SQUYRRL_S3_SECRET_KEY"`
	S3UseSSL    bool   `env:"SQUYRRL_S3_USE_SSL" envDefault:"false"`

	// 邮件走 Resend HTTP API。ResendAPIKey 为空时进入本地日志模式（不外发，见 auth.Mailer）。
	ResendAPIKey string `env:"SQUYRRL_RESEND_API_KEY"`
	MailFrom     string `env:"SQUYRRL_MAIL_FROM" envDefault:"Squyrrl <noreply@squyrrl.com>"`

	// 内部 server-to-server token，按端点分权（最小权限）：
	//   TGInternalToken 守 /internal/tg（Bot 绑定/转发碎片）；InternalToken 守 /internal/admin
	//   （发币、改第三方 endpoint 密钥等高危）。取不同值：Bot 凭证泄漏也碰不到管理端。
	//   任一为空 ⇒ 对应端点组整体 401（fail-closed，见 tg.InternalAuth）。
	TGInternalToken string `env:"SQUYRRL_TG_INTERNAL_TOKEN"`
	InternalToken   string `env:"SQUYRRL_INTERNAL_TOKEN"`

	// Bot 的 @username（不带 @），用于拼绑定 deep link t.me/<name>?start=<token>。
	// 由服务端拼而不是客户端硬编码：换 Bot 只改一处环境变量，不用发三端的版本。
	// 留空 ⇒ 签发绑定链接的端点返回 503（见 tg.Service.IssueBindingLink），
	// 其余 TG 功能不受影响。
	TGBotUsername string `env:"SQUYRRL_TG_BOT_USERNAME"`

	// WebAuthn / Passkey
	WebAuthnRPID    string   `env:"SQUYRRL_WEBAUTHN_RP_ID"     envDefault:"localhost"`
	WebAuthnRPName  string   `env:"SQUYRRL_WEBAUTHN_RP_NAME"   envDefault:"Squyrrl"`
	WebAuthnOrigins []string `env:"SQUYRRL_WEBAUTHN_ORIGINS"   envSeparator:"," envDefault:"http://localhost:10260,http://localhost:3000"`

	// 后台任务节奏
	FileGCInterval time.Duration `env:"SQUYRRL_FILE_GC_INTERVAL" envDefault:"5m"`

	// 每次 URI 解析请求扣减的 credits（命中/未命中一致，见 parser.Service.Parse）；<=0 = 不扣。
	// 跨环境统一走默认值 2，故不在 .env 模板出现；dev 想免费解析可显式设 0 覆盖。
	ParseCost int64 `env:"SQUYRRL_PARSE_COST" envDefault:"2"`

	// 订阅 webhook 签名密钥（任一为空表示该 provider 不启用）
	StripeWebhookSecret string `env:"SQUYRRL_STRIPE_WEBHOOK_SECRET"`
	AppleSharedSecret   string `env:"SQUYRRL_APPLE_SHARED_SECRET"`
	GooglePubsubAud     string `env:"SQUYRRL_GOOGLE_PUBSUB_AUD"` // RTDN OIDC token 期望的 audience
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) IsProd() bool { return c.Env == "prod" }
