// mockupstream 以独立进程运行 mock 上游，用于手动调试和压测。
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/eeee0717/llm-gateway/internal/mockupstream"
)

func main() {
	addr := flag.String("addr", ":9090", "监听地址")
	var opts mockupstream.Options
	flag.IntVar(&opts.Tokens, "tokens", 20, "回答的 token 数")
	flag.DurationVar(&opts.FirstDelay, "first-delay", 300*time.Millisecond, "第一个 token 之前的等待")
	flag.DurationVar(&opts.TokenDelay, "token-delay", 30*time.Millisecond, "相邻两个 token 之间的等待")
	flag.IntVar(&opts.FailStatus, "fail-status", 0, "非 0 时直接返回这个状态码的错误")
	flag.IntVar(&opts.AbortAfter, "abort-after", 0, "非 0 时输出这么多个 token 后断开连接")
	flag.Parse()

	srv := &http.Server{Addr: *addr, Handler: mockupstream.New(opts), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("mock upstream listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("mock upstream stopped", "error", err)
		os.Exit(1)
	}
}
