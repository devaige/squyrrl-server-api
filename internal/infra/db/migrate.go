package db

import (
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// Migrate 在启动时执行所有未应用的向上迁移。
// 迁移文件以 embed 方式打包进二进制，部署时无需额外文件。
func Migrate(pool *pgxpool.Pool) error {
	connCfg := pool.Config().ConnConfig
	sqlDB := stdlib.OpenDB(*connCfg)
	defer sqlDB.Close()

	goose.SetBaseFS(embeddedMigrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, "migrations")
}
