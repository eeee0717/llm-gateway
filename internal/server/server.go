// Package server 装配 Gin 引擎和公共中间件，负责监听端口和优雅退出。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/admin"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/requestid"
)

const (
	// shutdownTimeout 是优雅退出时等待进行中请求的最长时间。
	shutdownTimeout = 30 * time.Second
	// settleGrace 是取消进行中的请求之后，留给它们结算的时间。
	settleGrace = 5 * time.Second
)

// New 返回业务端口的处理器。auth 校验调用方的 API Key，排在公共中间件之后。
func New(logger *slog.Logger, rh *relay.Handler, auth gin.HandlerFunc) http.Handler {
	r := engine(logger, auth)
	r.POST("/v1/chat/completions", rh.ChatCompletions)
	r.GET("/v1/models", rh.Models)
	return r
}

// NewAdmin 返回管理端口的处理器。auth 校验管理员密钥，排在公共中间件之后。
func NewAdmin(logger *slog.Logger, ah *admin.Handler, auth gin.HandlerFunc) http.Handler {
	r := engine(logger, auth)
	r.POST("/admin/keys", ah.CreateKey)
	r.GET("/admin/keys/:id", ah.GetKey)
	r.POST("/admin/keys/:id/credit", ah.Credit)
	r.POST("/admin/keys/:id/limit", ah.Limit)
	r.POST("/admin/keys/:id/disable", ah.Disable)
	return r
}

// engine 建一个带公共中间件的 Gin 引擎：请求 ID 放最前，访问日志和后面的处理才取得到它。
func engine(logger *slog.Logger, mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(append([]gin.HandlerFunc{requestid.Middleware(), accessLog(logger), recovery(logger)}, mw...)...)
	return r
}

// Listener 是一个监听：业务端口或管理端口。
type Listener struct {
	Name    string // 只用于日志
	Addr    string
	Handler http.Handler
}

// Run 同时提供这些监听上的服务，直到 ctx 被取消，或者其中一个监听出错；
// 之后不再接受新请求，并等进行中的请求处理完（包括结算）。
// 等过 shutdownTimeout 还没结束的请求会被取消，它们按"调用方中途断开"的规则结算，不会留下没结算的预扣。
func Run(ctx context.Context, logger *slog.Logger, listeners ...Listener) error {
	// 进行中的请求都从 baseCtx 派生，退出时靠取消它来收尾
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	servers := make([]*http.Server, 0, len(listeners))
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		srv := &http.Server{
			Addr:              l.Addr,
			Handler:           l.Handler,
			BaseContext:       func(net.Listener) context.Context { return baseCtx },
			ReadHeaderTimeout: 10 * time.Second,
			// 不设 WriteTimeout：它从读完请求头开始计时，会截断长时间的流式响应。
			// IdleTimeout 不设时沿用 ReadTimeout，而 ReadTimeout 也没设，空闲的 keep-alive 连接就永远不会关闭。
			IdleTimeout: 2 * time.Minute,
		}
		servers = append(servers, srv)
		go func() { errc <- srv.ListenAndServe() }()
		logger.Info("listening", "name", l.Name, "addr", l.Addr)
	}

	var err error
	select {
	case err = <-errc: // 有一个监听起不来（例如端口被占用），另一个也一起收摊
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	if e := shutdown(servers, shutdownTimeout); e != nil {
		// 只有超时才值得取消进行中的请求；其他错误（比如关监听失败）再等一轮也没用
		if errors.Is(e, context.DeadlineExceeded) {
			logger.Warn("shutdown timed out, canceling in-flight requests", "error", e)
			cancelRequests()
			_ = shutdown(servers, settleGrace)
		} else {
			logger.Warn("shutdown failed", "error", e)
		}
		if err == nil {
			err = e
		}
	}
	return err
}

// shutdown 同时关掉所有监听，并等进行中的请求结束。同时关是因为一个一个来的话，
// 前一个等待期间后一个还在收新请求。
func shutdown(servers []*http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = srv.Shutdown(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// accessLog 在每个请求结束后记一条访问日志。它只读 c.Writer 的状态，不包装 ResponseWriter，流式刷新不受影响。
func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.InfoContext(c.Request.Context(), "request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
	}
}

// recovery 把 handler 里的 panic 转成 500 错误响应，并用 slog 记下堆栈。
// 连接已断开这类 panic 由 Gin 自己吞掉，不会走到这里。
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, err any) {
		logger.ErrorContext(c.Request.Context(), "panic recovered", "error", err, "stack", string(debug.Stack()))
		c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
	})
}
