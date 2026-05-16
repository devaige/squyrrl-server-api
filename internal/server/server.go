package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/squyrrl/api/internal/config"
	"github.com/squyrrl/api/internal/features/archive"
	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/claim"
	"github.com/squyrrl/api/internal/features/file"
	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/parser"
	"github.com/squyrrl/api/internal/features/search"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/subscriptions"
	"github.com/squyrrl/api/internal/features/tag"
	"github.com/squyrrl/api/internal/features/tg"
	"github.com/squyrrl/api/internal/features/wallet"
	"github.com/squyrrl/api/internal/infra/storage"
)

type Server struct {
	cfg     *config.Config
	pool    *pgxpool.Pool
	engine  *gin.Engine
	authSvc *auth.Service
	pageSvc *page.Service
	tagRepo *tag.Repo
	snipSvc *snippet.Service
	fileSvc    *file.Service
	parsSvc    *parser.Service
	tgSvc      *tg.Service
	passkeySvc *auth.PasskeyService
	walletSvc  *wallet.Service
	archiveSvc *archive.Service
	subSvc     *subscriptions.Service
	claimSvc   *claim.Service
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

	authSvc := auth.NewService(
		auth.NewRepo(pool),
		auth.NewMailer(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPFrom),
	)
	pageSvc := page.NewService(page.NewRepo(pool))
	tagRepo := tag.NewRepo(pool)
	snipSvc := snippet.NewService(snippet.NewRepo(pool))
	if meili := search.NewMeili(cfg.MeiliURL, cfg.MeiliKey, cfg.MeiliIndex); meili != nil {
		snipSvc.SetIndexer(search.NewMeiliIndexer(meili))
		// 启动时同步 index 设置；失败仅 warn，不阻塞启动
		go func() {
			if err := meili.EnsureIndex(context.Background()); err != nil {
				slog.Warn("meili EnsureIndex failed", "err", err)
			}
		}()
	}
	fileSvc := file.NewService(file.NewRepo(pool), st)

	// URI 解析：特殊 provider 顺序匹配；通用 OG 兜底放在最末
	registry := parser.NewRegistry()
	registry.Register(parser.NewYouTubeProvider())
	registry.Register(parser.NewGistProvider())
	registry.Register(parser.NewRedditProvider())
	registry.Register(parser.NewGenericOGProvider())
	walletSvc := wallet.NewService(wallet.NewRepo(pool))
	parsSvc := parser.NewService(registry, parser.NewCache(pool), walletSvc, cfg.ParseCost)

	tgSvc := tg.NewService(tg.NewRepo(pool), snipSvc)
	passkeySvc := auth.NewPasskeyService(wa, auth.NewPasskeySessionStore(), authSvc)
	archiveSvc := archive.NewService(pool, snipSvc, fileSvc, buildArchiveRenderers(cfg.ArchiveRenderers)...)
	subSvc := subscriptions.NewService(subscriptions.NewRepo(pool), walletSvc)
	claimSvc := claim.NewService(pool)

	s := &Server{
		cfg: cfg, pool: pool, engine: engine,
		authSvc: authSvc, pageSvc: pageSvc, tagRepo: tagRepo,
		snipSvc: snipSvc, fileSvc: fileSvc, parsSvc: parsSvc, tgSvc: tgSvc,
		passkeySvc: passkeySvc,
		walletSvc:  walletSvc,
		archiveSvc: archiveSvc,
		subSvc:     subSvc,
		claimSvc:   claimSvc,
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.engine }

// buildArchiveRenderers 按 cfg.ArchiveRenderers 数组按序构造 renderer。
// 未知名字静默忽略；空数组退化为 light。
func buildArchiveRenderers(names []string) []archive.Renderer {
	out := make([]archive.Renderer, 0, len(names))
	for _, n := range names {
		switch n {
		case "light", "":
			out = append(out, archive.NewLightRenderer())
		case "chromedp":
			out = append(out, archive.NewChromeRenderer())
		}
	}
	if len(out) == 0 {
		out = append(out, archive.NewLightRenderer())
	}
	return out
}

func (s *Server) routes() {
	s.engine.GET("/health", s.handleHealth)

	// auth：公开 + 鉴权两组
	authHandler := auth.NewHandler(s.authSvc)
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
	tag.NewHandler(s.tagRepo).Register(api.Group("/tags"))
	snippet.NewHandler(s.snipSvc).Register(api.Group("/snippets"))
	file.NewHandler(s.fileSvc).Register(api.Group("/files"))
	parser.NewHandler(s.parsSvc).Register(api.Group("/uris"))
	archive.NewHandler(s.archiveSvc).Register(api) // 注册到 /snippets/:id/archive

	tgHandler := tg.NewHandler(s.tgSvc)
	tgHandler.RegisterUser(api.Group("/tg"))

	// 钱包 — 用户端在 /me/wallet；匿名数据归属在 /me/anonymous/claim
	meGroup := api.Group("/me")
	walletHandler := wallet.NewHandler(s.walletSvc)
	walletHandler.RegisterUser(meGroup)
	claim.NewHandler(s.claimSvc).Register(meGroup)

	// 内部端：受 X-Internal-Token 头保护，不挂 Bearer 中间件
	internal := s.engine.Group("/internal")
	internal.Use(tg.InternalAuth(s.cfg.TGInternalToken))
	tgHandler.RegisterInternal(internal.Group("/tg"))
	walletHandler.RegisterInternal(internal.Group("/admin"))

	// 订阅 webhook：必须放在 Bearer 中间件之外（外部支付平台无法持有用户 token）
	subscriptions.NewHandler(
		s.subSvc,
		s.cfg.StripeWebhookSecret,
		s.cfg.AppleSharedSecret,
		s.cfg.GooglePubsubAud,
	).Register(s.engine.Group("/webhooks"))
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
