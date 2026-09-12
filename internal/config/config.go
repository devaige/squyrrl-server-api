package config

import (
	"fmt"
	"strings"
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
	// 发信域用产品域 squyrrl.com（2026-09-09 改回）：Resend 免费版现已支持验证多个域，
	// 当初改用组织级 lumixord.com 的唯一理由（免费版只能验一个域）不再成立。
	// SPF / DKIM / DMARC 三条记录相应配在 squyrrl.com 区 —— From 域与验证域不一致时
	// 邮件直接进垃圾箱，这是换域时唯一会咬人的地方。
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

	// 客户端与边缘之间的直连（ADR-069 上传 / ADR-070 下载）。两项**都必填**，
	// 任一为空 ⇒ 上传与下载一起不可用（fail-closed）。
	//   UploadEdgeBase：边缘基址。生产填 Worker 的自定义域（https://files.squyrrl.com）；
	//     dev 填 api 自己的内嵌边缘（http://localhost:10260/edge，宿主端口而非容器内的
	//     8080：这个值原样发给客户端当收发终点）——Worker 的 R2 binding
	//     连不到 MinIO，wrangler dev 的本地 R2 是另一套存储，所以本地跑 Worker 走不通。
	//     刻意**没有**「留空则回退到服务器中转」这一档：那一档会让文件字节穿过 api，
	//     一次配置疏漏就变成持续的出网带宽账单，而它坏得毫无声响。宁可传不了，不可悄悄花钱。
	//   UploadTokenSecret：api 与边缘共享的 HMAC 密钥，两侧必须同值否则边缘一律 401。
	//     上传票与下载票同密钥不同用途，签名互不通用（见 file/token.go）。
	// TG 的服务端中转（/internal/tg/files）不依赖这两项，永远可用。
	UploadEdgeBase string `env:"SQUYRRL_UPLOAD_EDGE_BASE"`
	// envDefault 与下方 devUploadTokenSecret 必须同值（Go 的 tag 只能写字面量）。
	UploadTokenSecret string `env:"SQUYRRL_UPLOAD_TOKEN_SECRET" envDefault:"dev-upload-token-secret"`

	// OTP 发信防滥用三层（ADR-074）。全部走 envDefault、不进 .env 模板 ——
	// 与 SQUYRRL_PARSE_COST 同例：跨环境统一，要调时显式覆盖即可。任一项 <=0 即关闭该层。
	//   Cooldown    同一邮箱的冷却窗口
	//   IPLimit/Window  单 IP 在窗口内允许的请求数（进程内计数，重启清零）
	//   DailyBudget 滚动 24 小时的发信上限。Resend 免费版 100 封/天，留 20 封余量。
	//     熔断时真实用户也登不进来，所以它是最后一道而非第一道 —— 触发即需人工介入。
	OTPCooldown    time.Duration `env:"SQUYRRL_OTP_COOLDOWN"     envDefault:"60s"`
	OTPIPLimit     int           `env:"SQUYRRL_OTP_IP_LIMIT"     envDefault:"10"`
	OTPIPWindow    time.Duration `env:"SQUYRRL_OTP_IP_WINDOW"    envDefault:"1h"`
	OTPDailyBudget int           `env:"SQUYRRL_OTP_DAILY_BUDGET" envDefault:"80"`

	// 验证侧的闸门（与上面三层不同轴：那三层管发信，这个管猜解）。
	// 单个邮箱在一个码周期内允许猜错的次数，用尽即销毁该邮箱下所有存活的码。
	// 取 5：6 位码猜中概率 5/10^6，而真实用户连错 5 次已属罕见。
	OTPMaxAttempts int `env:"SQUYRRL_OTP_MAX_ATTEMPTS" envDefault:"5"`

	// 后台任务节奏
	FileGCInterval time.Duration `env:"SQUYRRL_FILE_GC_INTERVAL" envDefault:"5m"`
	// 回收站清理不需要频繁：最短保留期是 30 天，6 小时的粒度对用户完全无感，
	// 而更密的轮询只是在反复扫同一批不到期的行。
	TrashSweepInterval time.Duration `env:"SQUYRRL_TRASH_SWEEP_INTERVAL" envDefault:"6h"`
	// 超额数据的标记/解除比回收站清理更需要及时：用户在宽限期里删掉足够多的数据
	// 之后，应当很快看到限制解除，而不是等上几个小时才确认自己的操作有效。
	RestrictionSweepInterval time.Duration `env:"SQUYRRL_RESTRICTION_SWEEP_INTERVAL" envDefault:"1h"`
	// 辅助表保留期清理（parse_cache 过期行 + api_samples 的线上采样，migration 000019）。
	// 一天一轮足够：过期的缓存行不影响任何正确性（Get 本来就带 expires_at 条件），
	// 多留一天只是多占一天磁盘，而更密的轮询只是在反复扫同一批还没到期的行。
	RetentionSweepInterval time.Duration `env:"SQUYRRL_RETENTION_SWEEP_INTERVAL" envDefault:"24h"`

	// 解析缓存的保留期。30 天是「同一条链接被再次解析的现实窗口」的量级 ——
	// 更长只是在替整个互联网存档（GenericOG 是兜底 provider，任意 http(s) 链接都会落一行），
	// 而缓存未命中的代价只是一次上游调用，不是数据丢失。
	// 设 0 可回到旧的「永不过期」行为，但那正是 000019 要修的东西。
	ParseCacheTTL time.Duration `env:"SQUYRRL_PARSE_CACHE_TTL" envDefault:"720h"`

	// 每次 URI 解析请求扣减的 credits（命中/未命中一致，见 parser.Service.Parse）；<=0 = 不扣。
	// 跨环境统一走默认值 2，故不在 .env 模板出现；dev 想免费解析可显式设 0 覆盖。
	ParseCost int64 `env:"SQUYRRL_PARSE_COST" envDefault:"2"`

	// 订阅 webhook 签名密钥（任一为空表示该 provider 不启用）
	StripeWebhookSecret string `env:"SQUYRRL_STRIPE_WEBHOOK_SECRET"`
	// StripeSecretKey 用于调 Stripe API 创建 Checkout Session。留空 ⇒ 购买入口返回 503
	// （fail-closed，与边缘直传同一个态度：没有一条「退化成别的方式收款」的暗路）。
	StripeSecretKey string `env:"SQUYRRL_STRIPE_SECRET_KEY"`
	// StripePrices 是 SKU → Stripe price id 的 JSON 映射，键为 `kind:tier:period`
	// （代币加购的 period 固定为 `once`）。例：
	//   {"plan:basic:monthly":"price_1A...","storage:s50:yearly":"price_1B...","credits:p5:once":"price_1C..."}
	//
	// 放环境变量而不是数据库：**Stripe 的测试模式与正式模式是两套完全不同的 price id**，
	// 它属于环境配置而非业务数据。放进库里意味着每次换环境都要改数据，
	// 而那正是环境变量存在的理由。
	StripePrices string `env:"SQUYRRL_STRIPE_PRICES"`
	// CheckoutReturnBase 是结账完成/取消后跳回的站点前缀，例如 https://squyrrl.com。
	CheckoutReturnBase string `env:"SQUYRRL_CHECKOUT_RETURN_BASE" envDefault:"https://squyrrl.com"`
	AppleSharedSecret  string `env:"SQUYRRL_APPLE_SHARED_SECRET"`
	GooglePubsubAud    string `env:"SQUYRRL_GOOGLE_PUBSUB_AUD"` // RTDN OIDC token 期望的 audience
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) IsProd() bool { return c.Env == "prod" }

const devUploadTokenSecret = "dev-upload-token-secret"

// Validate 拦截「能正常启动、却会悄悄失效或悄悄花钱」的生产配置组合。
//
// 边缘的 fail-closed 设计意味着漏配 EDGE_BASE 只会让上传/下载 503，不会退回服务器中转 ——
// 安全，但也安静：不在启动时喊出来，就要等第一个用户传不了/打不开文件才发现。
// 只在 prod 收紧，dev 沿用默认值即可开箱即用。
func (c *Config) Validate() error {
	if !c.IsProd() {
		return nil
	}
	var bad []string
	if c.UploadEdgeBase == "" {
		bad = append(bad, "SQUYRRL_UPLOAD_EDGE_BASE 未设置：客户端上传与下载将整体不可用")
	}
	if c.UploadTokenSecret == "" || c.UploadTokenSecret == devUploadTokenSecret {
		bad = append(bad, "SQUYRRL_UPLOAD_TOKEN_SECRET 未覆盖 dev 默认值")
	}
	if len(bad) > 0 {
		return fmt.Errorf("生产配置不合法：%s", strings.Join(bad, "；"))
	}
	return nil
}
