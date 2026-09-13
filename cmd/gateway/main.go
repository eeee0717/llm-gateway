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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/eeee0717/llm-gateway/internal/admin"
	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/billing"
	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/ratelimit"
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
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		return nil, err
	}
	db, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	// 一个实例最多占 20 条连接。PostgreSQL 默认只允许 100 条，多起几个实例也不会把连接占满；
	// 并发再高也只是在这 20 条上排队，预扣本来就是同一行上的串行操作。
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	// 连接用满一小时就换掉：数据库重启、主从切换或者中间的连接跟踪超时后，
	// 池子里可能留着一批已经不通的连接，靠它们自然轮换掉。
	db.SetConnMaxLifetime(time.Hour)
	return gdb, nil
}

// openRedis 建立 Redis 客户端。连接是按需建立的，所以 Redis 没起来也不影响网关启动：
// 鉴权缓存会直接回源数据库，限流会放行，见 docs/adr/0002。
func openRedis(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	return redis.NewClient(opts), nil
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
	rdb, err := openRedis(cfg.Redis.URL)
	if err != nil {
		return err
	}
	defer rdb.Close()
	pingRedis(rdb, logger)

	business, management := build(cfg, db, rdb, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, logger,
		server.Listener{Name: "business", Addr: cfg.Listen, Handler: business},
		server.Listener{Name: "admin", Addr: cfg.Admin.Listen, Handler: management},
	)
}

// pingRedis 在启动时探一下 Redis，连不上只记一条日志：网关照常提供服务，
// 只是鉴权缓存和限流暂时不起作用。
func pingRedis(rdb *redis.Client, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Warn("redis is not reachable, auth cache and rate limit stay off until it recovers", "error", err)
		return
	}
	logger.Info("redis connected")
}

// build 组装两个端口的处理器。所有依赖都在这里接起来，测试也用它，测的就是真正跑起来的那套装配。
func build(cfg *config.Config, db *gorm.DB, rdb *redis.Client, logger *slog.Logger) (business, management http.Handler) {
	keys := apikey.NewStore(db)
	cache := apikey.NewCache(rdb, keys, logger)
	limiter := ratelimit.New(rdb, time.Now, logger)
	rh := relay.New(cfg, biller{billing.New(db, prices(cfg))}, logger)
	ah := admin.New(logger, keys, cache)
	return server.New(logger, rh,
			apikey.Middleware(logger, cache),
			ratelimit.Middleware(limiter, caller(cfg.RateLimit.DefaultRPM))),
		server.NewAdmin(logger, ah, admin.Auth(cfg.Admin.Key))
}

// caller 把鉴权的结果变成限流认的调用方：额度用这个 Key 自己设的，没设就用配置里的默认值。
// 限流不认识 API Key 是怎么来的，两个包在这里接起来。
func caller(defaultRPM int) ratelimit.Caller {
	return func(ctx context.Context) (int64, int, bool) {
		identity, ok := apikey.From(ctx)
		if !ok {
			return 0, 0, false
		}
		rpm := identity.RPM
		if rpm <= 0 {
			rpm = defaultRPM
		}
		return identity.ID, rpm, true
	}
}

// biller 把 billing.Service 接到 relay.Biller 上。两个包各自定义自己的类型，谁也不 import 谁，
// 转换放在这里：两边的字段一样，直接转就行，哪天对不上了就是一个编译错误。
type biller struct{ svc *billing.Service }

func (b biller) Reserve(ctx context.Context, r relay.Reservation) (int64, bool, error) {
	return b.svc.Reserve(ctx, billing.Reservation(r))
}

func (b biller) Settle(ctx context.Context, r relay.Result) error {
	return b.svc.Settle(ctx, billing.Result(r))
}

// prices 把配置里每个模型的单价整理成计费用的表。
func prices(cfg *config.Config) map[string]billing.Price {
	p := make(map[string]billing.Price, len(cfg.Models))
	for _, m := range cfg.Models {
		p[m.Name] = billing.PricePerMillionTokens(m.InputPrice, m.OutputPrice)
	}
	return p
}
