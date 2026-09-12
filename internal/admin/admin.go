// Package admin 是管理接口：创建 API Key、充值、禁用、查询余额。
// 它监听单独的端口，凭管理员密钥访问；管理员密钥和调用方的 API Key 互不通用。
package admin

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/openai"
)

// Handler 处理 /admin 下的接口。
type Handler struct {
	keys   *apikey.Store
	logger *slog.Logger
}

func New(logger *slog.Logger, keys *apikey.Store) *Handler {
	return &Handler{keys: keys, logger: logger}
}

// Auth 校验管理员密钥。比较用固定时间，避免按耗时逐字节猜出密钥。
func Auth(adminKey string) gin.HandlerFunc {
	want := []byte(adminKey)
	return func(c *gin.Context) {
		got := []byte(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				openai.NewError(openai.TypeInvalidRequest, "invalid_admin_key", "invalid admin key"))
			return
		}
		c.Next()
	}
}

// CreateKey 处理 POST /admin/keys：新建一个 Key，余额为零。
func (h *Handler) CreateKey(c *gin.Context) {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			openai.NewError(openai.TypeInvalidRequest, "invalid_request", "name is required"))
		return
	}
	plain, hash := apikey.Generate()
	key, err := h.keys.Create(c.Request.Context(), body.Name, hash)
	if err != nil {
		h.internalError(c, "create api key", err)
		return
	}
	c.JSON(http.StatusCreated, view(key, plain))
}

// keyResponse 是 Key 的对外表示。key 字段只在创建时有值，明文不会再出现第二次。
type keyResponse struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Key          string    `json:"key,omitempty"`
	BalanceMicro int64     `json:"balance_micro"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
}

func view(k apikey.Key, plain string) keyResponse {
	return keyResponse{
		ID:           k.ID,
		Name:         k.Name,
		Key:          plain,
		BalanceMicro: k.BalanceMicro,
		Disabled:     k.Disabled,
		CreatedAt:    k.CreatedAt,
	}
}

// internalError 记下错误并返回 500，不把数据库的错误信息透给外面。
func (h *Handler) internalError(c *gin.Context, msg string, err error) {
	h.logger.ErrorContext(c.Request.Context(), msg, "error", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError,
		openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
}
