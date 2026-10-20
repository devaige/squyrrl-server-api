package server

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/config"
	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/parser"
	"github.com/squyrrl/api/internal/features/pricing"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/replica"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/tag"
	"github.com/squyrrl/api/internal/features/tg"
	"github.com/squyrrl/api/internal/features/users"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/ratelimit"
	"github.com/squyrrl/api/internal/infra/storage"
)

type Server struct {
	cfg         *config.Config
	pool        *pgxpool.Pool
	engine      *gin.Engine
	authSvc     *auth.Service
	pageSvc     *page.Service
	tagRepo     *tag.Repo
	checkoutSvc *subscriptions.Checkout
	snipSvc     *snippet.Service
	fileSvc     *file.Service
	parsSvc     *parser.Service
	tgSvc       *tg.Service
	passkeySvc  *auth.PasskeyService
	walletSvc   *wallet.Service
	subSvc      *subscriptions.Service
	extapiSvc   *extapi.Service
	parseCache  *parser.Cache
	quotaSvc    *quota.Service
}

func New(cfg *config.Config, pool *pgxpool.Pool, st *storage.Client) *Server {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Recovery())

	// WebAuthn 配置
	wconfig := &webauthn.Config{
		RPDisplayName: cfg.WebAuthnRPName,
		RPID:          cfg.WebAuthnRPID,
		RPOrigins:     cfg.WebAuthnOrigins,
	}
	wa, err := webauthn.New(wconfig)
	if err != nil {
		// 配置无效时拒绝启动，让 ops 立刻发现问题。
		panic("webauthn 配置无效：" + err.Error())
	}

	// wallet 与 quota 要先于所有业务 service 构造：门槛检查需要读用户档位，
	// 而档位来自 wallet（subscriptions 表）。auth 也在其列 —— 登录时要按档位
	// 逐出超额设备，所以它同样排在这两者之后。
	walletSvc := wallet.NewService(wallet.NewRepo(pool))
	quotaSvc := quota.NewService(pool, walletSvc)

	authSvc := auth.NewService(
		auth.NewRepo(pool),
		auth.NewMailer(cfg.ResendAPIKey, cfg.MailFrom),
		auth.OTPGuard{
			Cooldown:    cfg.OTPCooldown,
			DailyBudget: cfg.OTPDailyBudget,
			MaxAttempts: cfg.OTPMaxAttempts,
		},
		quotaSvc,
	)

	pageSvc := page.NewService(page.NewRepo(pool), quotaSvc)
	tagRepo := tag.NewRepo(pool)
	snipSvc := snippet.NewService(snippet.NewRepo(pool), quotaSvc)
	fileSvc := file.NewService(file.NewRepo(pool), st, quotaSvc, cfg.UploadEdgeBase, cfg.UploadTokenSecret)

	// URI 解析（ADR-080）：平台按注册顺序匹配链接；每个平台的来源按价格从低到高
	// 排成兜底链 —— 内置免费来源在前，付费上游接口（api_endpoints + 代码适配器）按单价升序。
	// oEmbed / OG 不算解析，没有「任意链接」的兜底平台。
	registry := parser.NewRegistry()
	registry.Register(parser.NewYouTubeProvider())
	registry.Register(parser.NewGistProvider())
	registry.Register(parser.NewRedditProvider())
	for _, p := range parser.NewUpstreamPlatforms() {
		registry.Register(p)
	}
	adapters := parser.DefaultAdapters()
	extapiSvc := extapi.NewService(extapi.NewRepo(pool),
		extapi.Credentials{TikHub: cfg.TikHubAPIKey, RapidAPI: cfg.RapidAPIKey}, adapters)
	parseCache := parser.NewCache(pool, cfg.ParseCacheTTL)
	parsSvc := parser.NewService(parser.Deps{
		Registry:        registry,
		Cache:           parseCache,
		Wallet:          walletSvc,
		Extapi:          extapiSvc,
		Adapters:        adapters,
		Prices:          parser.NewPriceRepo(pool),
		Logs:            parser.NewLogRepo(pool),
		DefaultPrice:    cfg.ParseCost,
		RefreshCooldown: cfg.ParseRefreshCooldown,
	})

	tgSvc := tg.NewService(tg.NewRepo(pool), snipSvc, quotaSvc, cfg.TGBotUsername)
	passkeySvc := auth.NewPasskeyService(wa, auth.NewPasskeySessionStore(), authSvc)
	// 档位升上去时立刻重算受限标记；定时清扫（main.go）照常兜底
	subSvc := subscriptions.NewService(subscriptions.NewRepo(pool), walletSvc,
		snippet.NewRestrictionSweeper(pool, walletSvc, 0))

	// 价目表解析失败只让购买入口不可用，不让进程起不来：
	// 一个填错的环境变量不该把收款问题放大成一次全站故障。
	priceBook, err := subscriptions.NewPriceBook(cfg.StripePrices)
	if err != nil {
		slog.Error("Stripe 价目表解析失败，购买入口将不可用", "err", err)
	}
	checkoutSvc := subscriptions.NewCheckout(cfg.StripeSecretKey, priceBook, cfg.CheckoutReturnBase)

	s := &Server{
		cfg: cfg, pool: pool, engine: engine,
		authSvc: authSvc, pageSvc: pageSvc, tagRepo: tagRepo,
		snipSvc: snipSvc, fileSvc: fileSvc, parsSvc: parsSvc, tgSvc: tgSvc,
		checkoutSvc: checkoutSvc,
		passkeySvc:  passkeySvc,
		walletSvc:   walletSvc,
		subSvc:      subSvc,
		extapiSvc:   extapiSvc,
		parseCache:  parseCache,
		quotaSvc:    quotaSvc,
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.engine }

// FileService 供 main 启动后台任务用（上传意图清理）。
func (s *Server) FileService() *file.Service { return s.fileSvc }

// WalletService 供后台任务读取用户档位（回收站清理要按档位定保留期）。
func (s *Server) WalletService() *wallet.Service { return s.walletSvc }

// ParseCache / ExtapiService 供 main 启动辅助表的保留期清理（migration 000019）。
// 两张表都不是业务数据，但都只增不减，而托管 Postgres 按容量计费。
func (s *Server) ParseCache() *parser.Cache { return s.parseCache }

func (s *Server) ExtapiService() *extapi.Service { return s.extapiSvc }

func (s *Server) routes() {
	// GET 与 HEAD 都要注册：**Gin 不会为 GET 路由自动响应 HEAD**（httprouter 的行为，
	// 与 net/http 的 ServeMux 不同），漏了这条 HEAD /health 会返回 404。
	// 而多数拨测服务默认就发 HEAD —— 那会表现为「服务明明是好的，监控一直报宕机」。
	s.engine.GET("/health", s.handleHealth)
	s.engine.HEAD("/health", s.handleHealth)

	// auth：公开 + 鉴权两组
	authHandler := auth.NewHandler(s.authSvc,
		ratelimit.New(s.cfg.OTPIPLimit, s.cfg.OTPIPWindow))
	authHandler.RegisterPublic(s.engine.Group("/auth"))
	authedAuth := s.engine.Group("/auth")
	authedAuth.Use(s.authSvc.Middleware())
	authHandler.RegisterAuthed(authedAuth)

	// passkey：公开（login）+ 鉴权（register）两组
	passkeyHandler := auth.NewPasskeyHandler(s.passkeySvc)
	passkeyHandler.RegisterPublic(s.engine.Group("/auth/passkey"))
	authedPasskey := s.engine.Group("/auth/passkey")
	authedPasskey.Use(s.authSvc.Middleware())
	passkeyHandler.RegisterAuthed(authedPasskey)

	// 业务路由统一挂在已鉴权的根 group 下
	api := s.engine.Group("/")
	api.Use(s.authSvc.Middleware())

	page.NewHandler(s.pageSvc).Register(api.Group("/pages"))
	tag.NewHandler(s.tagRepo, s.quotaSvc).Register(api.Group("/tags"))
	snippet.NewHandler(s.snipSvc).Register(api.Group("/snippets"))
	// 原生客户端的增量同步（ADR-084）；网页端与扩展仍走上面三组 REST 接口
	replica.NewHandler(replica.NewService(s.pool, s.quotaSvc, s.snipSvc, s.pageSvc, s.tagRepo)).
		Register(api.Group("/sync"))
	file.NewHandler(s.fileSvc).Register(api.Group("/files"))
	parserHandler := parser.NewHandler(s.parsSvc)
	parserHandler.Register(api.Group("/uris")) // POST /uris/parse（鉴权 + 计费）
	// GET /uris/manifest 走公开 group（无 Bearer 中间件）：匿名客户端也需要清单做本地判断
	parserHandler.RegisterPublic(s.engine.Group("/uris"))

	// GET /pricing 同样公开：购买页与「帮我选择」向导在登录前就要能算价（ADR-075）。
	// 它也是唯一能不发版调价的机制，客户端冷启动拉一次。
	pricing.NewHandler().RegisterPublic(s.engine.Group("/"))

	// 钱包 — 用户端在 /me/wallet
	meGroup := api.Group("/me")
	walletHandler := wallet.NewHandler(s.walletSvc, s.quotaSvc)
	walletHandler.RegisterUser(meGroup)

	// TG 绑定归属于「我的账户设置」，故挂 /me/tg 而非顶层 /tg：
	// 兑换码、列出已绑 TG 号、解绑，都是对当前登录用户自身的操作。
	tgHandler := tg.NewHandler(s.tgSvc, s.fileSvc)
	tgHandler.RegisterUser(meGroup.Group("/tg"))

	// 内部端：受 X-Internal-Token 头保护，不挂 Bearer 中间件。
	// 按最小权限拆两个守卫（tg.InternalAuth 是通用的 header 比对器，可复用于不同 token）：
	// Bot 只持 TG token（够用 /internal/tg）；管理后台持独立 InternalToken 才能碰 /internal/admin
	// （发币、改 endpoint 密钥等高危）。两段互不越权，Bot 凭证泄漏不波及管理端。
	internal := s.engine.Group("/internal")

	tgInternal := internal.Group("/tg")
	tgInternal.Use(tg.InternalAuth(s.cfg.TGInternalToken))
	tgHandler.RegisterInternal(tgInternal)

	adminInternal := internal.Group("/admin")
	adminInternal.Use(tg.InternalAuth(s.cfg.InternalToken))
	walletHandler.RegisterInternal(adminInternal)
	extapi.NewHandler(s.extapiSvc).RegisterInternal(adminInternal)
	parser.NewHandler(s.parsSvc).RegisterInternal(adminInternal)
	users.NewHandler(users.NewService(users.NewRepo(s.pool), s.walletSvc, s.quotaSvc, s.subSvc)).RegisterInternal(adminInternal)
	subscriptions.NewAdminHandler(s.subSvc).RegisterInternal(adminInternal)
	snippet.NewAdminHandler(snippet.NewAdminRepo(s.pool)).RegisterInternal(adminInternal)
	file.NewHandler(s.fileSvc).RegisterInternal(adminInternal)

	// 订阅 webhook：必须放在 Bearer 中间件之外（外部支付平台无法持有用户 token）。
	// 购买入口则相反 —— 它要知道是谁在买，挂在 /me 上。
	playGuard := subscriptions.PlayGuard{
		PackageName: s.cfg.GooglePackageName,
		AllowTest:   s.cfg.GooglePlayEnvironment == "Test",
	}
	// 凭据坏掉不阻断启动：Play 收单会 503，其它渠道照常。反过来（panic 退出）
	// 意味着一个只影响 Android 购买的配置错误能让整个 API 起不来。
	playAPI, err := subscriptions.NewPlayAPI(playGuard, s.cfg.GooglePlayServiceAccount)
	if err != nil {
		slog.Warn("Google Play 收单未启用", "err", err)
	}
	subHandler := subscriptions.NewHandler(subscriptions.Deps{
		Service:      s.subSvc,
		Checkout:     s.checkoutSvc,
		StripeSecret: s.cfg.StripeWebhookSecret,
		AppleGuard: subscriptions.AppleGuard{
			BundleID:    s.cfg.AppleBundleID,
			Environment: s.cfg.AppleEnvironment,
		},
		GoogleOIDC: subscriptions.NewGoogleOIDCVerifier(
			s.cfg.GooglePubsubAud, s.cfg.GooglePubsubEmail, nil),
		PlayGuard: playGuard,
		PlayAPI:   playAPI,
	})
	subHandler.Register(s.engine.Group("/webhooks"))
	subHandler.RegisterUser(meGroup)
}

func (s *Server) handleHealth(c *gin.Context) {
	if err := s.pool.Ping(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "down",
			"db":     err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"env":    "prod",
	})
}
