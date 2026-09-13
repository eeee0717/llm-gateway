// Package admin 是管理接口：创建 API Key、充值、禁用、查询余额。
// 它监听单独的端口，凭管理员密钥访问；管理员密钥和调用方的 API Key 互不通用。
package admin

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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

// Auth 校验管理员密钥。Authorization 头必须写成 Bearer 形式，裸密钥不算数。
// 密钥本身的比较用固定时间，避免按耗时逐字节猜出密钥。
func Auth(adminKey string) gin.HandlerFunc {
	want := []byte(adminKey)
	return func(c *gin.Context) {
		got, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
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
		Name     string `json:"name"`
		RPMLimit int    `json:"rpm_limit"` // 可选，0 表示用配置里的默认额度
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == "" || body.RPMLimit < 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			openai.NewError(openai.TypeInvalidRequest, "invalid_request", "name is required and rpm_limit must not be negative"))
		return
	}
	plain, hash := apikey.Generate()
	key, err := h.keys.Create(c.Request.Context(), body.Name, hash, body.RPMLimit)
	if err != nil {
		h.internalError(c, "create api key", err)
		return
	}
	c.JSON(http.StatusCreated, view(key, plain))
}

// GetKey 处理 GET /admin/keys/:id：查余额和状态。
func (h *Handler) GetKey(c *gin.Context) {
	h.respond(c, func(id int64) (apikey.Key, error) {
		return h.keys.ByID(c.Request.Context(), id)
	})
}

// Credit 处理 POST /admin/keys/:id/credit：给余额充值，金额的单位是微元。
func (h *Handler) Credit(c *gin.Context) {
	var body struct {
		AmountMicro int64 `json:"amount_micro"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.AmountMicro <= 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			openai.NewError(openai.TypeInvalidRequest, "invalid_request", "amount_micro must be positive"))
		return
	}
	h.respond(c, func(id int64) (apikey.Key, error) {
		return h.keys.Credit(c.Request.Context(), id, body.AmountMicro)
	})
}

// Limit 处理 POST /admin/keys/:id/limit：改这个 Key 的限流额度，0 表示改回跟着配置走。
func (h *Handler) Limit(c *gin.Context) {
	var body struct {
		RPMLimit int `json:"rpm_limit"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.RPMLimit < 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			openai.NewError(openai.TypeInvalidRequest, "invalid_request", "rpm_limit must not be negative"))
		return
	}
	h.respond(c, func(id int64) (apikey.Key, error) {
		return h.keys.SetRPMLimit(c.Request.Context(), id, body.RPMLimit)
	})
}

// Disable 处理 POST /admin/keys/:id/disable：停用一个 Key，余额保留。
func (h *Handler) Disable(c *gin.Context) {
	h.respond(c, func(id int64) (apikey.Key, error) {
		return h.keys.Disable(c.Request.Context(), id)
	})
}

// respond 解析路径里的 Key ID，执行 do，再把结果写回。
func (h *Handler) respond(c *gin.Context, do func(id int64) (apikey.Key, error)) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			openai.NewError(openai.TypeInvalidRequest, "invalid_request", "key id must be an integer"))
		return
	}
	key, err := do(id)
	switch {
	case errors.Is(err, apikey.ErrNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound,
			openai.NewError(openai.TypeInvalidRequest, "key_not_found", "api key not found"))
	case err != nil:
		h.internalError(c, "admin request failed", err)
	default:
		c.JSON(http.StatusOK, view(key, ""))
	}
}

// keyResponse 是 Key 的对外表示。key 字段只在创建时有值，明文不会再出现第二次。
type keyResponse struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Key          string    `json:"key,omitempty"`
	BalanceMicro int64     `json:"balance_micro"`
	Disabled     bool      `json:"disabled"`
	RPMLimit     int       `json:"rpm_limit"`
	CreatedAt    time.Time `json:"created_at"`
}

func view(k apikey.Key, plain string) keyResponse {
	return keyResponse{
		ID:           k.ID,
		Name:         k.Name,
		Key:          plain,
		BalanceMicro: k.BalanceMicro,
		Disabled:     k.Disabled,
		RPMLimit:     k.RPMLimit,
		CreatedAt:    k.CreatedAt,
	}
}

// internalError 记下错误并返回 500，不把数据库的错误信息透给外面。
func (h *Handler) internalError(c *gin.Context, msg string, err error) {
	h.logger.ErrorContext(c.Request.Context(), msg, "error", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError,
		openai.NewError(openai.TypeServer, "internal_error", "internal server error"))
}
