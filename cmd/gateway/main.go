// gateway 是网关的可执行文件。
//
//	gateway serve   [-config config.yaml]   启动网关
//	gateway migrate [-config config.yaml]   把数据库迁移到最新版本
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/eeee0717/llm-gateway/internal/admin"
	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/billing"
	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/requestid"
	"github.com/eeee0717/llm-gateway/internal/server"
	"github.com/eeee0717/llm-gateway/migrations"
)

const usage = "usage: gateway serve|migrate [-config config.yaml]"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "migrate":
		return migrate(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// loadConfig 解析子命令共用的 -config 参数并加载配置。
func loadConfig(command string, args []string) (*config.Config, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	path := fs.String("config", "config.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

// migrate 把数据库迁移到最新版本。迁移已经应用过的库不会出错，可以重复执行。
func migrate(args []string) error {
	cfg, err := loadConfig("migrate", args)
	if err != nil {
		return err
	}
	gdb, err := openDB(cfg.Database.DSN)
	if err != nil {
		return err
	}
	db, err := gdb.DB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrations.Up(db); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

// openDB 连接 PostgreSQL。GORM 自带的日志会直接打到标准输出、和 JSON 日志混在一起，所以关掉。
func openDB(dsn string) (*gorm.DB, error) {
	return gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Discard})
}

// serve 加载配置、组装依赖并启动网关，收到 SIGINT 或 SIGTERM 后优雅退出。
func serve(args []string) error {
	cfg, err := loadConfig("serve", args)
	if err != nil {
		return err
	}
	logger := slog.New(requestid.LogHandler(slog.NewJSONHandler(os.Stdout, nil)))
	db, err := openDB(cfg.Database.DSN)
	if err != nil {
		return err
	}

	keys := apikey.NewStore(db)
	rh := relay.New(cfg, billing.New(logger, db, prices(cfg)), logger)
	ah := admin.New(logger, keys)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, logger,
		server.Listener{Name: "business", Addr: cfg.Listen, Handler: server.New(logger, rh, apikey.Middleware(logger, keys))},
		server.Listener{Name: "admin", Addr: cfg.Admin.Listen, Handler: server.NewAdmin(logger, ah, admin.Auth(cfg.Admin.Key))},
	)
}

// prices 把配置里每个模型的单价整理成计费用的表。
func prices(cfg *config.Config) map[string]billing.Price {
	p := make(map[string]billing.Price, len(cfg.Models))
	for _, m := range cfg.Models {
		p[m.Name] = billing.Price{Input: m.InputPrice, Output: m.OutputPrice}
	}
	return p
}
