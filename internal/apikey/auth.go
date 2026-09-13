package apikey

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

type ctxKey struct{}

// NewContext 把鉴权的结果放进 context。后面的限流和计费从这里取，不碰 Key 的明文。
func NewContext(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, identity)
}

// From 取出当前请求的鉴权结果。没有经过鉴权时返回 false。
func From(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(ctxKey{}).(Identity)
	return identity, ok
}

// Lookup 按 Key 的哈希查出鉴权要用的信息。Store 和它的缓存 Cache 都实现了这个接口，
// 装不装缓存由 cmd/gateway 决定。
type Lookup interface {
	IdentityByHash(ctx context.Context, hash string) (Identity, error)
}

// Middleware 校验 API Key：从 Authorization 头取出明文，按 SHA-256 查出对应的 Key，
// 通过后把鉴权结果放进 context。Key 不存在和被禁用分开报错，方便调用方知道是该换 Key 还是该找管理员。
func Middleware(logger *slog.Logger, keys Lookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		plain, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || plain == "" {
			unauthorized(c, "invalid_api_key", "missing API key")
			return
		}
		identity, err := keys.IdentityByHash(c.Request.Context(), Hash(plain))
		switch {
		case errors.Is(err, ErrNotFound):
			unauthorized(c, "invalid_api_key", "invalid API key")
			return
		case err != nil:
			logger.ErrorContext(c.Request.Context(), "look up api key", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
			return
		case identity.Disabled:
			unauthorized(c, "key_disabled", "API key is disabled")
			return
		}
		c.Request = c.Request.WithContext(NewContext(c.Request.Context(), identity))
		c.Next()
	}
}

func unauthorized(c *gin.Context, code, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, openai.NewError(openai.TypeInvalidRequest, code, message))
}
