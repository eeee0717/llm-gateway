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

// NewContext 把 Key 的 ID 放进 context。日志和计费只认 ID，不碰明文。
func NewContext(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From 取出当前请求的 Key ID。没有经过鉴权时返回 false。
func From(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok
}

// Middleware 校验 API Key：从 Authorization 头取出明文，按 SHA-256 查库，通过后把 Key 的 ID 放进 context。
// Key 不存在和被禁用分开报错，方便调用方知道是该换 Key 还是该找管理员。
func Middleware(logger *slog.Logger, store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		plain, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || plain == "" {
			unauthorized(c, "invalid_api_key", "missing API key")
			return
		}
		key, err := store.ByHash(c.Request.Context(), Hash(plain))
		switch {
		case errors.Is(err, ErrNotFound):
			unauthorized(c, "invalid_api_key", "invalid API key")
			return
		case err != nil:
			logger.ErrorContext(c.Request.Context(), "look up api key", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
			return
		case key.Disabled:
			unauthorized(c, "key_disabled", "API key is disabled")
			return
		}
		c.Request = c.Request.WithContext(NewContext(c.Request.Context(), key.ID))
		c.Next()
	}
}

func unauthorized(c *gin.Context, code, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, openai.NewError(openai.TypeInvalidRequest, code, message))
}
