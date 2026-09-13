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
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/internal/mockupstream"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/testdb"
	"github.com/eeee0717/llm-gateway/internal/testredis"
)

const adminKey = "admin-secret"

// 一次请求的两个金额。消息只有 1 个字符，网关估算和 mock 上游报告的 prompt token 都是 1；
// 模型的默认输出上限是 5，而 mock 只回 3 个 token，所以每次结算都要退回 16 微元的差额。
const (
	reservedPerRequest = 1*2 + 5*8 // 转发前按输出上限预扣
	costPerRequest     = 1*2 + 3*8 // 结束后按实际用量结算
)

// M2 收口断言：两个网关实例共用一套 PostgreSQL，1000 个并发请求打同一个 API Key，余额零超扣。
//
// 余额只够预扣 500 次。退回的差额会让后面的请求又扣得下，所以成功的次数不是一个定值，
// 但有两条性质必须成立：余额不能变成负数（零超扣），而且余额加上所有用量记录的费用必须正好等于充值额
// （账目守恒）。少退、多退、重复结算都会破坏守恒，扣穿会破坏前一条。
func TestTwoInstancesDoNotOverspendOneKey(t *testing.T) {
	const (
		requests = 1000
		credited = 500 * reservedPerRequest
	)
	db := testdb.New(t) // 顺带把迁移跑好
	logger := slog.New(slog.DiscardHandler)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	cfg := testConfig(upstream.URL)

	first, second := startInstance(t, cfg, logger), startInstance(t, cfg, logger)
	key, keyID := createKey(t, first, credited)

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
	require.EqualValues(t, requests, succeeded.Load()+rejected.Load())

	balance := balanceOf(t, db, keyID)
	records, cost := usageOf(t, db, keyID)
	require.GreaterOrEqual(t, balance, int64(0))         // 零超扣
	require.EqualValues(t, credited, balance+cost)       // 账目守恒
	require.EqualValues(t, succeeded.Load(), records)    // 每个成功的请求留下一条用量记录
	require.EqualValues(t, records*costPerRequest, cost) // 每条记录的费用都是实际用量算出来的

	// 成功的次数落在一个算得出来的区间里：最少是按预扣金额能支付的次数（退款一次都还没发生），
	// 最多是按实际费用能支付的次数（每次预扣之前差额都已经退回来了）。
	require.GreaterOrEqual(t, succeeded.Load(), int64(credited/reservedPerRequest))
	require.LessOrEqual(t, succeeded.Load(), int64(credited/costPerRequest))
}

// 流式请求同样走预扣和结算，用量取上游在流末尾报告的那份。
func TestStreamingRequestIsBilledFromUpstreamUsage(t *testing.T) {
	const credited = 1_000_000
	db := testdb.New(t)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	gw := startInstance(t, testConfig(upstream.URL), slog.New(slog.DiscardHandler))
	key, keyID := createKey(t, gw, credited)

	status, body := chatStream(t, gw.business, key)

	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"content":"The"`) // 每个 token 一个事件
	require.Contains(t, body, "[DONE]")
	records, cost := usageOf(t, db, keyID)
	require.EqualValues(t, 1, records)
	require.EqualValues(t, costPerRequest, cost) // 按实际用量，不是按预扣的输出上限
	require.EqualValues(t, credited-costPerRequest, balanceOf(t, db, keyID))
}

// M3 收口断言：Key 被禁用后立即失效，不用等鉴权缓存过期。
// 在一个实例上禁用，另一个实例的下一个请求就该被拒——两个实例共用同一套 Redis，
// 禁用时删掉的是那份共享的缓存。
func TestDisabledKeyStopsWorkingImmediatelyOnEveryInstance(t *testing.T) {
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	cfg := testConfig(upstream.URL)
	logger := slog.New(slog.DiscardHandler)
	first, second := startInstance(t, cfg, logger), startInstance(t, cfg, logger)
	key, keyID := createKey(t, first, 1_000_000)

	// 两个实例各跑一次，把这个 Key 灌进各自看到的那份缓存
	require.Equal(t, http.StatusOK, chat(t.Context(), first.business, key))
	require.Equal(t, http.StatusOK, chat(t.Context(), second.business, key))

	adminPost(t, first.admin, fmt.Sprintf("/admin/keys/%d/disable", keyID), ``, nil)

	for name, gw := range map[string]instance{"same instance": first, "other instance": second} {
		t.Run(name, func(t *testing.T) {
			status, code := chatOnce(t, gw.business, key)
			require.Equal(t, http.StatusUnauthorized, status)
			require.Equal(t, "key_disabled", code) // 说明是鉴权挡下的，不是预扣挡下的
		})
	}
}

// 鉴权缓存确实挡在数据库前面，而且只有走管理接口的改动才能立刻穿透它。
// 绕过管理接口直接改库，缓存里的旧身份还在用——这就是 docs/notes/auth-cache.md 里
// 那个陈旧窗口，它的后果是被禁用的 Key 落到预扣上被挡下（429），而不是鉴权挡下（401）。
func TestAuthCacheHoldsTheIdentityUntilAdminInvalidatesIt(t *testing.T) {
	db := testdb.New(t)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	gw := startInstance(t, testConfig(upstream.URL), slog.New(slog.DiscardHandler))
	key, keyID := createKey(t, gw, 1_000_000)
	require.Equal(t, http.StatusOK, chat(t.Context(), gw.business, key)) // 把身份灌进缓存

	require.NoError(t, db.Exec(`UPDATE api_keys SET disabled = TRUE WHERE id = ?`, keyID).Error)

	status, code := chatOnce(t, gw.business, key)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "insufficient_quota", code) // 鉴权放行了，是预扣挡下的

	adminPost(t, gw.admin, fmt.Sprintf("/admin/keys/%d/disable", keyID), ``, nil)

	status, code = chatOnce(t, gw.business, key)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, "key_disabled", code)
}

// 改额度同样立刻生效：改完删缓存，下一个请求读到的就是新额度。
func TestChangingTheQuotaTakesEffectImmediately(t *testing.T) {
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	gw := startInstance(t, testConfig(upstream.URL), slog.New(slog.DiscardHandler))
	key, keyID := createKey(t, gw, 1_000_000)
	require.Equal(t, http.StatusOK, chat(t.Context(), gw.business, key)) // 灌缓存，这时额度还是配置里的默认值

	adminPost(t, gw.admin, fmt.Sprintf("/admin/keys/%d/limit", keyID), `{"rpm_limit":1}`, nil)

	// 容量一改小，桶里的令牌也被压到新容量，所以只剩一个
	require.Equal(t, http.StatusOK, chat(t.Context(), gw.business, key))
	status, code := chatOnce(t, gw.business, key)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, "rate_limit_exceeded", code) // 缓存没删的话，这里用的还是那个很大的默认额度
}

// M3 收口断言：限流放行的请求总数正确。
// 把这个 Key 的额度设成每分钟 6 个，瞬间打 300 个请求：桶一开始是满的，正好放行 6 个，其余都被拒。
//
// 这里用的是真实时钟，所以额度要取小：额度 6 意味着每 10 秒才补回一个令牌，
// 整个测试跑完连一秒都不到，补充的量远不足一个，放行数就是个确定的数。
// 额度取 60 的话每秒就补一个，测试稍微慢一点就会多放行一个。
// 精确到"一个令牌"的补充行为由 internal/ratelimit 的测试用注入的时钟去断言。
func TestRateLimitAllowsExactlyTheKeyQuota(t *testing.T) {
	const (
		requests = 300
		quota    = 6
	)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	gw := startInstance(t, testConfig(upstream.URL), slog.New(slog.DiscardHandler))
	key, keyID := createKey(t, gw, 1_000_000_000) // 余额给够，被拒只可能是限流
	adminPost(t, gw.admin, fmt.Sprintf("/admin/keys/%d/limit", keyID), fmt.Sprintf(`{"rpm_limit":%d}`, quota), nil)

	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch chat(t.Context(), gw.business, key) {
			case http.StatusOK:
				allowed.Add(1)
			case http.StatusTooManyRequests:
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	require.EqualValues(t, quota, allowed.Load())
	require.EqualValues(t, requests-quota, rejected.Load())
	_, code := chatOnce(t, gw.business, key)
	require.Equal(t, "rate_limit_exceeded", code) // 是限流拒的，不是余额不够
}

// Redis 连不上时网关照常工作：鉴权直接查数据库，限流放行，计费一点不受影响——
// 余额只在 PostgreSQL，Redis 里的东西丢了都能重建，见 docs/adr/0002。
func TestGatewayKeepsWorkingWithoutRedis(t *testing.T) {
	const credited = 1_000_000
	db := testdb.New(t)
	upstream := httptest.NewServer(mockupstream.New(mockupstream.Options{Tokens: 3}))
	t.Cleanup(upstream.Close)
	cfg := testConfig(upstream.URL)
	logger := slog.New(slog.DiscardHandler)
	healthy := startInstance(t, cfg, logger)
	key, keyID := createKey(t, healthy, credited)

	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { down.Close() })
	broken := startInstanceWith(t, cfg, down, logger)

	require.Equal(t, http.StatusOK, chat(t.Context(), broken.business, key))
	require.EqualValues(t, credited-costPerRequest, balanceOf(t, db, keyID))
}

// instance 是一个网关实例的两个端口。
type instance struct {
	business string
	admin    string
}

// startInstance 起一个网关实例。每个实例自己开一套数据库连接，和真正跑两个进程一样。
func startInstance(t *testing.T, cfg *config.Config, logger *slog.Logger) instance {
	t.Helper()
	return startInstanceWith(t, cfg, testredis.New(t), logger)
}

func startInstanceWith(t *testing.T, cfg *config.Config, rdb *redis.Client, logger *slog.Logger) instance {
	t.Helper()
	db, err := openDB(testdb.DSN())
	require.NoError(t, err)
	business, management := build(cfg, db, rdb, logger)
	b := httptest.NewServer(business)
	t.Cleanup(b.Close)
	m := httptest.NewServer(management)
	t.Cleanup(m.Close)
	return instance{business: b.URL, admin: m.URL}
}

func testConfig(upstreamURL string) *config.Config {
	return &config.Config{
		Admin: config.Admin{Key: adminKey},
		// 默认额度给得很大，只有专门测限流的用例才去设 Key 自己的额度
		RateLimit: config.RateLimit{DefaultRPM: 100_000},
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

// chatOnce 发一个非流式请求，返回状态码和错误响应里的 code，成功时 code 为空。
func chatOnce(t *testing.T, url, key string) (int, string) {
	t.Helper()
	body := `{"model":"mock-model","messages":[{"role":"user","content":"a"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	var failure openai.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&failure)
	return resp.StatusCode, failure.Error.Code
}

// chatStream 发一个流式请求，读完整个流，返回状态码和收到的内容。
func chatStream(t *testing.T, url, key string) (int, string) {
	t.Helper()
	body := `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"a"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	received, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(received)
}

// balanceOf 查一个 Key 的余额。查不到行时 Scan 不报错、balance 留在零值，所以这里要看行数：
// 否则"Key 被删了"和"余额是 0"分不开。
func balanceOf(t *testing.T, db *gorm.DB, keyID int64) int64 {
	t.Helper()
	var balance int64
	res := db.Raw(`SELECT balance_micro FROM api_keys WHERE id = ?`, keyID).Scan(&balance)
	require.NoError(t, res.Error)
	require.EqualValues(t, 1, res.RowsAffected, "api key %d not found", keyID)
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
