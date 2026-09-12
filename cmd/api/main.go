package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"github.com/squyrrl/api/internal/config"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/infra/db"
	"github.com/squyrrl/api/internal/infra/storage"
	"github.com/squyrrl/api/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		logger.Error("配置校验失败", "err", err)
		os.Exit(1)
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	pool, err := db.Connect(rootCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("连接数据库失败", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(pool); err != nil {
		logger.Error("数据库迁移失败", "err", err)
		os.Exit(1)
	}
	logger.Info("数据库迁移完成")

	// 对象存储
	parsedEndpoint := cfg.S3Endpoint
	parsedEndpoint = trimScheme(parsedEndpoint)
	st, err := storage.New(parsedEndpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		logger.Error("对象存储初始化失败", "err", err)
		os.Exit(1)
	}
	if err := st.EnsureBucket(rootCtx); err != nil {
		logger.Error("对象存储 bucket 检查失败", "err", err)
		os.Exit(1)
	}

	// 后台任务：file GC（异步清理 ref_count=0 的文件）
	gc := storage.NewGC(pool, st, cfg.FileGCInterval)
	go gc.Run(rootCtx)

	srv := server.New(cfg, pool, st)

	// 后台任务：清理过期未收尾的直传意图（ADR-069）。
	// 与上面的 file GC 是两类垃圾：GC 收的是 files 表里 ref_count 归零的行，
	// 这里收的是「客户端拿了令牌却没 commit」——那些字节在 R2 里，files 表却没有行，
	// 唯一的线索就是意图表。
	go srv.FileService().RunIntentSweeper(rootCtx, cfg.FileGCInterval)

	// 后台任务：按档位清理过期的回收站条目（ADR-075 ⑯）。
	// 放在 server.New 之后是因为它要用 walletSvc 解析「用户现在算哪一档」——
	// 那段带宽限期的逻辑只该有一个定义。
	go snippet.NewTrashSweeper(pool, srv.WalletService(), cfg.TrashSweepInterval).Run(rootCtx)

	// 后台任务：维护降级后超额数据的生命周期（ADR-075，2026-09-11 用户决策）。
	// 与回收站清理是两条链路：那边处理用户自己删过的，这边处理用户没删、
	// 但已经不在档位额度内的。跑得比回收站勤，因为用户在宽限期内删数据之后
	// 应当尽快恢复正常，而不是等到几小时后的下一轮。
	go snippet.NewRestrictionSweeper(pool, srv.WalletService(), cfg.RestrictionSweepInterval).Run(rootCtx)

	// 后台任务：两张辅助表的保留期（migration 000019）。
	// 与上面三条的区别是它们回收的不是用户数据，而是「越受欢迎越大、且没有上界」的
	// 平台自用数据 —— 解析缓存的上界是整个互联网，线上采样的上界是调用次数。
	// 两者都不影响正确性，所以跑得最稀疏，也因此共用同一个间隔。
	go srv.ParseCache().RunSweeper(rootCtx, cfg.RetentionSweepInterval)
	go srv.ExtapiService().RunSampleSweeper(rootCtx, cfg.RetentionSweepInterval)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("HTTP 服务启动", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "err", err)
			cancelRoot()
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		logger.Info("收到关闭信号，开始优雅停机")
	case <-rootCtx.Done():
		logger.Warn("内部错误触发停机")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP 优雅停机失败", "err", err)
	}
	logger.Info("已退出")
}

// trimScheme 去除可能存在的 http(s):// 前缀，minio-go endpoint 仅接 host:port
func trimScheme(s string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}
