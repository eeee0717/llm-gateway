// gateway 是网关的可执行文件。
//
//	gateway serve [-config config.yaml]   启动网关
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

	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/requestid"
	"github.com/eeee0717/llm-gateway/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway serve [-config config.yaml]")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// serve 加载配置、组装依赖并启动网关，收到 SIGINT 或 SIGTERM 后优雅退出。
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(requestid.LogHandler(slog.NewJSONHandler(os.Stdout, nil)))

	rh := relay.New(cfg, logBiller{logger}, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, logger, cfg.Listen, server.New(logger, rh))
}

// logBiller 在接入计费之前顶替结算：只把每次请求的用量写进日志。
type logBiller struct {
	logger *slog.Logger
}

func (b logBiller) Settle(ctx context.Context, r relay.Result) error {
	b.logger.InfoContext(ctx, "usage",
		"model", r.Model,
		"prompt_tokens", r.Usage.PromptTokens,
		"completion_tokens", r.Usage.CompletionTokens,
		"estimated", r.Estimated,
	)
	return nil
}
