package server

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/config"
	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/claim"
	"github.com/squyrrl/api/internal/features/extapi"
	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/parser"
	"github.com/squyrrl/api/internal/features/pricing"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/tag"
	"github.com/squyrrl/api/internal/features/tg"
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
	claimSvc    *claim.Service
	extapiSvc   *extapi.Service
	parseCache  *parser.Cache
	quotaSvc    *quota.Service
}

func New(cfg *config.Config, pool *pgxpool.Pool, st *storage.Client) *Server {
	if cfg.IsProd() {
		gin.SetMode(gin.ReleaseMode)
	}

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
		// dev 环境退化为空实现风险太高，这里直接 panic 让 ops 立刻发现配置问题
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

	// URI 解析：特殊 provider 顺序匹配；通用 OG 兜底放在最末
	registry := parser.NewRegistry()
	registry.Register(parser.NewYouTubeProvider())
	registry.Register(parser.NewGistProvider())
	registry.Register(parser.NewRedditProvider())
	registry.Register(parser.NewGenericOGProvider())
	extapiSvc := extapi.NewService(extapi.NewRepo(pool))
	parseCache := parser.NewCache(pool, cfg.ParseCacheTTL)
	parsSvc := parser.NewService(registry, parseCache, walletSvc, extapiSvc, cfg.ParseCost)

	tgSvc := tg.NewService(tg.NewRepo(pool), snipSvc, quotaSvc, cfg.TGBotUsername)
	passkeySvc := auth.NewPasskeyService(wa, auth.NewPasskeySessionStore(), authSvc)
	subSvc := subscriptions.NewService(subscriptions.NewRepo(pool), walletSvc)

	// 价目表解析失败只让购买入口不可用，不让进程起不来：
	// 一个填错的环境变量不该把收款问题放大成一次全站故障。
	priceBook, err := subscriptions.NewPriceBook(cfg.StripePrices)
	if err != nil {
		slog.Error("Stripe 价目表解析失败，购买入口将不可用", "err", err)
	}
	checkoutSvc := subscriptions.NewCheckout(cfg.StripeSecretKey, priceBook, cfg.CheckoutReturnBase)
	claimSvc := claim.NewService(pool, quotaSvc)

	s := &Server{
		cfg: cfg, pool: pool, engine: engine,
		authSvc: authSvc, pageSvc: pageSvc, tagRepo: tagRepo,
		snipSvc: snipSvc, fileSvc: fileSvc, parsSvc: parsSvc, tgSvc: tgSvc,
		checkoutSvc: checkoutSvc,
		passkeySvc:  passkeySvc,
		walletSvc:   walletSvc,
		subSvc:      subSvc,
		claimSvc:    claimSvc,
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

	// 内嵌边缘（ADR-069 上传 / ADR-070 下载）：**只在非 prod 注册**，且这是硬门闩，
	// 不看任何其它配置。这组端点用边缘令牌认证而非 Bearer，故挂在公开 group 上。
	//
	// 门闩按 Env 而非「是否配了 Worker」来开：后者意味着漏配一个环境变量就悄悄
	// 打开一条「绕开 CDN、改吃服务器出网带宽」的收发路径 —— 功能全对，只是每个字节
	// 都在计费，而且没有任何报错提示。生产要么走 Worker，要么传不了取不到，没有中间态。
	if !s.cfg.IsProd() {
		file.NewEdgeHandler(s.fileSvc).Register(s.engine.Group("/edge"))
	}

	// 业务路由统一挂在已鉴权的根 group 下
	api := s.engine.Group("/")
	api.Use(s.authSvc.Middleware())

	page.NewHandler(s.pageSvc).Register(api.Group("/pages"))
	tag.NewHandler(s.tagRepo, s.quotaSvc).Register(api.Group("/tags"))
	snippet.NewHandler(s.snipSvc).Register(api.Group("/snippets"))
	file.NewHandler(s.fileSvc).Register(api.Group("/files"))
	parserHandler := parser.NewHandler(s.parsSvc)
	parserHandler.Register(api.Group("/uris")) // POST /uris/parse（鉴权 + 计费）
	// GET /uris/manifest 走公开 group（无 Bearer 中间件）：匿名客户端也需要清单做本地判断
	parserHandler.RegisterPublic(s.engine.Group("/uris"))

	// GET /pricing 同样公开：购买页与「帮我选择」向导在登录前就要能算价（ADR-075）。
	// 它也是唯一能不发版调价的机制，客户端冷启动拉一次。
	pricing.NewHandler().RegisterPublic(s.engine.Group("/"))

	// 钱包 — 用户端在 /me/wallet；匿名数据归属在 /me/anonymous/claim
	meGroup := api.Group("/me")
	walletHandler := wallet.NewHandler(s.walletSvc, s.quotaSvc)
	walletHandler.RegisterUser(meGroup)
	claim.NewHandler(s.claimSvc).Register(meGroup)

	// GET /me/export：整批导出云端数据（ADR-075 降级生命周期的出口）。
	// 挂在 /me 而不是 /snippets：它导的是「我的全部数据」而不是碎片列表的一种查询，
	// 且将来页面、标签、附件清单都会进同一份文件。
	snippet.NewExportHandler(snippet.NewExporter(s.pool)).RegisterUser(meGroup)

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

	// 订阅 webhook：必须放在 Bearer 中间件之外（外部支付平台无法持有用户 token）。
	// 购买入口则相反 —— 它要知道是谁在买，挂在 /me 上。
	subHandler := subscriptions.NewHandler(
		s.subSvc,
		s.checkoutSvc,
		s.cfg.StripeWebhookSecret,
		s.cfg.AppleSharedSecret,
		s.cfg.GooglePubsubAud,
	)
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
		"env":    s.cfg.Env,
	})
}
