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
