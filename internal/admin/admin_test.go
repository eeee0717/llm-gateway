package admin_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/admin"
	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/server"
	"github.com/eeee0717/llm-gateway/internal/testdb"
)

const adminKey = "admin-secret"

func TestCreateKeyReturnsThePlainKeyOnce(t *testing.T) {
	a := startAdmin(t)

	resp := a.post(t, "/admin/keys", `{"name":"alice"}`, adminKey)

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeKey(t, resp)
	require.True(t, strings.HasPrefix(created.Key, "sk-"))
	require.Equal(t, "alice", created.Name)
	require.Zero(t, created.BalanceMicro) // 新建的 Key 余额为零
	require.False(t, created.Disabled)

	// 返回的明文对应库里那条记录，而库里存的是它的哈希
	var name string
	require.NoError(t, a.db.Raw(`SELECT name FROM api_keys WHERE key_hash = ?`, apikey.Hash(created.Key)).Scan(&name).Error)
	require.Equal(t, "alice", name)
}

func TestAdminRejectsRequestsWithoutTheAdminKey(t *testing.T) {
	a := startAdmin(t)

	for _, tc := range []struct{ name, key string }{
		{"no key", ""},
		{"wrong key", "admin-wrong"},
		{"api key instead of admin key", "sk-caller-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := a.post(t, "/admin/keys", `{"name":"mallory"}`, tc.key)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}

	var count int64
	require.NoError(t, a.db.Raw(`SELECT count(*) FROM api_keys WHERE name = ?`, "mallory").Scan(&count).Error)
	require.Zero(t, count) // 鉴权不通过时不会建出 Key
}

func TestCreateKeyRequiresName(t *testing.T) {
	a := startAdmin(t)

	resp := a.post(t, "/admin/keys", `{}`, adminKey)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreditAddsToTheBalance(t *testing.T) {
	a := startAdmin(t)
	created := decodeKey(t, a.post(t, "/admin/keys", `{"name":"alice"}`, adminKey))
	path := fmt.Sprintf("/admin/keys/%d/credit", created.ID)

	first := decodeKey(t, a.post(t, path, `{"amount_micro":1000000}`, adminKey))
	second := decodeKey(t, a.post(t, path, `{"amount_micro":500000}`, adminKey))

	require.EqualValues(t, 1_000_000, first.BalanceMicro)
	require.EqualValues(t, 1_500_000, second.BalanceMicro) // 充值是累加
	require.Empty(t, second.Key)                           // 明文只在创建时返回
}

func TestDisableKeepsTheBalance(t *testing.T) {
	a := startAdmin(t)
	created := decodeKey(t, a.post(t, "/admin/keys", `{"name":"alice"}`, adminKey))
	a.post(t, fmt.Sprintf("/admin/keys/%d/credit", created.ID), `{"amount_micro":1000000}`, adminKey)

	disabled := decodeKey(t, a.post(t, fmt.Sprintf("/admin/keys/%d/disable", created.ID), ``, adminKey))

	require.True(t, disabled.Disabled)
	require.EqualValues(t, 1_000_000, disabled.BalanceMicro) // 禁用只是停用，余额保留
}

func TestGetKeyReturnsBalanceWithoutThePlainKey(t *testing.T) {
	a := startAdmin(t)
	created := decodeKey(t, a.post(t, "/admin/keys", `{"name":"alice"}`, adminKey))

	got := decodeKey(t, a.get(t, fmt.Sprintf("/admin/keys/%d", created.ID), adminKey))

	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "alice", got.Name)
	require.Empty(t, got.Key) // 明文没有第二次机会
}

func TestAdminRejectsBadKeyIDs(t *testing.T) {
	a := startAdmin(t)

	require.Equal(t, http.StatusNotFound, a.get(t, "/admin/keys/999999999", adminKey).StatusCode)
	require.Equal(t, http.StatusBadRequest, a.get(t, "/admin/keys/abc", adminKey).StatusCode)
}

func TestCreditRequiresAPositiveAmount(t *testing.T) {
	a := startAdmin(t)
	created := decodeKey(t, a.post(t, "/admin/keys", `{"name":"alice"}`, adminKey))
	path := fmt.Sprintf("/admin/keys/%d/credit", created.ID)

	require.Equal(t, http.StatusBadRequest, a.post(t, path, `{"amount_micro":0}`, adminKey).StatusCode)
	require.Equal(t, http.StatusBadRequest, a.post(t, path, `{"amount_micro":-1}`, adminKey).StatusCode)
}

// keyView 是管理接口返回的 Key 信息。
type keyView struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	BalanceMicro int64  `json:"balance_micro"`
	Disabled     bool   `json:"disabled"`
}

func decodeKey(t *testing.T, resp *http.Response) keyView {
	t.Helper()
	var view keyView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&view))
	return view
}

type adminServer struct {
	url string
	db  *gorm.DB
}

func startAdmin(t *testing.T) *adminServer {
	t.Helper()
	db := testdb.New(t)
	logger := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(server.NewAdmin(logger, admin.New(logger, apikey.NewStore(db)), admin.Auth(adminKey)))
	t.Cleanup(srv.Close)
	return &adminServer{url: srv.URL, db: db}
}

// post 以管理员的身份发一个请求；key 为空表示不带 Authorization 头。
func (a *adminServer) post(t *testing.T, path, body, key string) *http.Response {
	t.Helper()
	return a.do(t, http.MethodPost, path, body, key)
}

func (a *adminServer) get(t *testing.T, path, key string) *http.Response {
	t.Helper()
	return a.do(t, http.MethodGet, path, "", key)
}

func (a *adminServer) do(t *testing.T, method, path, body, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, a.url+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
