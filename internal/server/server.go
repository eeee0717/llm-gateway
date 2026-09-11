// Package server 装配 Gin 引擎和公共中间件，负责监听端口和优雅退出。
package server

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/requestid"
)

// shutdownTimeout 是优雅退出时等待进行中请求的最长时间。
const shutdownTimeout = 30 * time.Second

// New 返回业务端口的 HTTP 处理器。
func New(logger *slog.Logger, rh *relay.Handler) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// requestid 放在最前面：访问日志和后面的 handler 都要从 context 里取请求 ID
	r.Use(requestid.Middleware(), accessLog(logger), recovery(logger))
	r.POST("/v1/chat/completions", rh.ChatCompletions)
	r.GET("/v1/models", rh.Models)
	return r
}

// Run 在 addr 上提供服务，直到 ctx 被取消；之后不再接受新请求，并等进行中的请求处理完（包括结算）。
func Run(ctx context.Context, logger *slog.Logger, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：它从读完请求头开始计时，会截断长时间的流式响应。
		// IdleTimeout 不设时沿用 ReadTimeout，而 ReadTimeout 也没设，空闲的 keep-alive 连接就永远不会关闭。
		IdleTimeout: 2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
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
