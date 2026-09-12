// Package server 装配 Gin 引擎和公共中间件，负责监听端口和优雅退出。
package server

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/admin"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/relay"
	"github.com/eeee0717/llm-gateway/internal/requestid"
)

// shutdownTimeout 是优雅退出时等待进行中请求的最长时间。
const shutdownTimeout = 30 * time.Second

// New 返回业务端口的处理器。
func New(logger *slog.Logger, rh *relay.Handler) http.Handler {
	r := engine(logger)
	r.POST("/v1/chat/completions", rh.ChatCompletions)
	r.GET("/v1/models", rh.Models)
	return r
}

// NewAdmin 返回管理端口的处理器。auth 校验管理员密钥，排在公共中间件之后。
func NewAdmin(logger *slog.Logger, ah *admin.Handler, auth gin.HandlerFunc) http.Handler {
	r := engine(logger, auth)
	r.POST("/admin/keys", ah.CreateKey)
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
func Run(ctx context.Context, logger *slog.Logger, listeners ...Listener) error {
	servers := make([]*http.Server, 0, len(listeners))
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		srv := &http.Server{
			Addr:              l.Addr,
			Handler:           l.Handler,
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if e := srv.Shutdown(shutdownCtx); e != nil && err == nil {
			err = e
		}
	}
	return err
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
