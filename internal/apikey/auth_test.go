package apikey_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/testdb"
)

func TestMiddlewarePassesTheKeyToTheHandler(t *testing.T) {
	store := apikey.NewStore(testdb.New(t))
	plain, hash := apikey.Generate()
	key, err := store.Create(t.Context(), "alice", hash, 0)
	require.NoError(t, err)
	url := startAuthed(t, store)

	resp := get(t, url, plain)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		KeyID int64 `json:"key_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, key.ID, got.KeyID) // 计费要按这个 ID 扣费
}

func TestMiddlewareRejectsBadKeys(t *testing.T) {
	store := apikey.NewStore(testdb.New(t))
	disabled, hash := apikey.Generate()
	key, err := store.Create(t.Context(), "bob", hash, 0)
	require.NoError(t, err)
	require.NoError(t, testdb.New(t).Exec(`UPDATE api_keys SET disabled = TRUE WHERE id = ?`, key.ID).Error)
	unknown, _ := apikey.Generate()
	url := startAuthed(t, store)

	for _, tc := range []struct {
		name string
		key  string
		code string
	}{
		{"no key", "", "invalid_api_key"},
		{"unknown key", unknown, "invalid_api_key"},
		{"admin key instead of api key", "admin-secret", "invalid_api_key"},
		{"disabled key", disabled, "key_disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := get(t, url, tc.key)

			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			var body openai.ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			require.Equal(t, tc.code, body.Error.Code)
		})
	}
}

// startAuthed 起一个只有鉴权中间件的服务，处理函数回显 context 里的 Key ID。
func startAuthed(t *testing.T, store *apikey.Store) string {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(apikey.Middleware(slog.New(slog.DiscardHandler), store))
	r.GET("/", func(c *gin.Context) {
		id, ok := apikey.From(c.Request.Context())
		require.True(t, ok)
		c.JSON(http.StatusOK, gin.H{"key_id": id})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// get 以调用方的身份发一个请求；key 为空表示不带 Authorization 头。
func get(t *testing.T, url, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
