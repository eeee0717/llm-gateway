package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/mockupstream"
	"github.com/eeee0717/llm-gateway/internal/testdb"
)

const adminKey = "admin-secret"

// M2 收口断言：两个网关实例共用一套 PostgreSQL，1000 个并发请求打同一个 API Key，余额零超扣。
//
// 每个请求的费用是确定的：消息只有 1 个字符，网关估算和 mock 上游报告的 prompt token 都是 1；
// 模型的默认输出上限是 5，mock 正好回 5 个 token，所以预扣和实际费用都是 1×2 + 5×8 = 42 微元。
// 余额只够 500 次，因此成功的次数必须正好是 500，最后余额必须正好是 0：
// 少扣会剩下钱，超扣会变成负数，两种都说明并发下的预扣出了问题。
func TestTwoInstancesDoNotOverspendOneKey(t *testing.T) {
	const (
		requests       = 1000
		affordable     = 500
		costPerRequest = 42
	)
	db := testdb.New(t) // 顺带把迁移跑好
	logger := slog.New(slog.DiscardHandler)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 5}))
	t.Cleanup(upstream.Close)
	cfg := testConfig(upstream.URL)

	first, second := startInstance(t, cfg, logger), startInstance(t, cfg, logger)
	key, keyID := createKey(t, first, affordable*costPerRequest)

	var succeeded, rejected, unexpected atomic.Int64
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			instance := first // 两个实例各打一半
			if i%2 == 1 {
				instance = second
			}
			switch chat(t.Context(), instance.business, key) {
			case http.StatusOK:
				succeeded.Add(1)
			case http.StatusTooManyRequests:
				rejected.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()

	require.Zero(t, unexpected.Load()) // 每个请求要么成功，要么因为余额不够被拒
	require.EqualValues(t, affordable, succeeded.Load())
	require.EqualValues(t, requests-affordable, rejected.Load())

	require.Zero(t, balanceOf(t, db, keyID))
	records, cost := usageOf(t, db, keyID)
	require.EqualValues(t, affordable, records) // 每个成功的请求留下一条用量记录
	require.EqualValues(t, affordable*costPerRequest, cost)
}

// instance 是一个网关实例的两个端口。
type instance struct {
	business string
	admin    string
}

// startInstance 起一个网关实例。每个实例自己开一套数据库连接，和真正跑两个进程一样。
func startInstance(t *testing.T, cfg *config.Config, logger *slog.Logger) instance {
	t.Helper()
	db, err := openDB(testdb.DSN())
	require.NoError(t, err)
	business, management := build(cfg, db, logger)
	b := httptest.NewServer(business)
	t.Cleanup(b.Close)
	m := httptest.NewServer(management)
	t.Cleanup(m.Close)
	return instance{business: b.URL, admin: m.URL}
}

func testConfig(upstreamURL string) *config.Config {
	return &config.Config{
		Admin:     config.Admin{Key: adminKey},
		Upstreams: []config.Upstream{{Name: "mock", BaseURL: upstreamURL + "/v1", Key: "upstream-key"}},
		Models: []config.Model{{
			Name:             "mock-model",
			Upstream:         "mock",
			InputPrice:       2,
			OutputPrice:      8,
			DefaultMaxTokens: 5,
		}},
	}
}

// createKey 通过管理接口创建一个 Key 并充值，返回明文和 ID。
func createKey(t *testing.T, on instance, balanceMicro int64) (string, int64) {
	t.Helper()
	var created struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	adminPost(t, on.admin, "/admin/keys", `{"name":"load test"}`, &created)
	adminPost(t, on.admin, fmt.Sprintf("/admin/keys/%d/credit", created.ID),
		fmt.Sprintf(`{"amount_micro":%d}`, balanceMicro), nil)
	return created.Key, created.ID
}

func adminPost(t *testing.T, url, path, body string, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	require.Less(t, resp.StatusCode, http.StatusMultipleChoices, path)
	if out != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
}

// chat 以调用方的身份发一个非流式请求，返回状态码。它跑在并发的 goroutine 里，所以不做断言。
func chat(ctx context.Context, url, key string) int {
	body := `{"model":"mock-model","messages":[{"role":"user","content":"a"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // 响应体读完，连接才能复用
	return resp.StatusCode
}

func balanceOf(t *testing.T, db *gorm.DB, keyID int64) int64 {
	t.Helper()
	var balance int64
	require.NoError(t, db.Raw(`SELECT balance_micro FROM api_keys WHERE id = ?`, keyID).Scan(&balance).Error)
	return balance
}

// usageOf 返回这个 Key 的用量记录条数和费用总额。
func usageOf(t *testing.T, db *gorm.DB, keyID int64) (count, cost int64) {
	t.Helper()
	var row struct {
		Count int64
		Cost  int64
	}
	require.NoError(t, db.Raw(
		`SELECT count(*) AS count, coalesce(sum(cost_micro), 0) AS cost FROM usage_records WHERE api_key_id = ?`,
		keyID).Scan(&row).Error)
	return row.Count, row.Cost
}
