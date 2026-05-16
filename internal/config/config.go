package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Env      string `env:"SQUYRRL_ENV"      envDefault:"dev"`
	HTTPAddr string `env:"SQUYRRL_HTTP_ADDR" envDefault:":8080"`

	DatabaseURL string `env:"SQUYRRL_DATABASE_URL,required"`

	RedisURL string `env:"SQUYRRL_REDIS_URL"`
	NATSURL  string `env:"SQUYRRL_NATS_URL"`

	S3Endpoint  string `env:"SQUYRRL_S3_ENDPOINT"`
	S3Bucket    string `env:"SQUYRRL_S3_BUCKET"`
	S3AccessKey string `env:"SQUYRRL_S3_ACCESS_KEY"`
	S3SecretKey string `env:"SQUYRRL_S3_SECRET_KEY"`
	S3UseSSL    bool   `env:"SQUYRRL_S3_USE_SSL" envDefault:"false"`

	SMTPHost string `env:"SQUYRRL_SMTP_HOST" envDefault:"localhost"`
	SMTPPort int    `env:"SQUYRRL_SMTP_PORT" envDefault:"51025"`
	SMTPUser string `env:"SQUYRRL_SMTP_USER"`
	SMTPPass string `env:"SQUYRRL_SMTP_PASS"`
	SMTPFrom string `env:"SQUYRRL_SMTP_FROM" envDefault:"Squyrrl <noreply@squyrrl.app>"`

	// TG Bot 与 API 之间的内部 token（仅 server-to-server，不暴露给客户端）
	TGInternalToken string `env:"SQUYRRL_TG_INTERNAL_TOKEN"`

	// WebAuthn / Passkey
	WebAuthnRPID     string   `env:"SQUYRRL_WEBAUTHN_RP_ID"     envDefault:"localhost"`
	WebAuthnRPName   string   `env:"SQUYRRL_WEBAUTHN_RP_NAME"   envDefault:"Squyrrl"`
	WebAuthnOrigins  []string `env:"SQUYRRL_WEBAUTHN_ORIGINS"   envSeparator:"," envDefault:"http://localhost:8080,http://localhost:3000"`

	// 后台任务节奏
	FileGCInterval time.Duration `env:"SQUYRRL_FILE_GC_INTERVAL" envDefault:"5m"`

	// URI 解析每次 cache-miss 扣减的 credits；<=0 时不强制（dev / free tier）
	ParseCost int64 `env:"SQUYRRL_PARSE_COST" envDefault:"2"`

	// URL 归档 renderer 链：'light' 单次 GET；'chromedp' headless Chrome inline 资源；
	// 'chromedp,light' 优先 chromedp，失败回退 light（推荐生产配置，需系统装 Chrome）
	ArchiveRenderers []string `env:"SQUYRRL_ARCHIVE_RENDERERS" envSeparator:"," envDefault:"light"`

	// 订阅 webhook 签名密钥（任一为空表示该 provider 不启用）
	StripeWebhookSecret string `env:"SQUYRRL_STRIPE_WEBHOOK_SECRET"`
	AppleSharedSecret   string `env:"SQUYRRL_APPLE_SHARED_SECRET"`
	GooglePubsubAud     string `env:"SQUYRRL_GOOGLE_PUBSUB_AUD"` // RTDN OIDC token 期望的 audience

	// Meilisearch 全文搜索（空 URL 时退化为 ILIKE）
	MeiliURL   string `env:"SQUYRRL_MEILI_URL"`
	MeiliKey   string `env:"SQUYRRL_MEILI_KEY"`
	MeiliIndex string `env:"SQUYRRL_MEILI_INDEX" envDefault:"squyrrl_snippets"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) IsProd() bool { return c.Env == "prod" }
