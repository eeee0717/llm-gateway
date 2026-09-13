// Package testredis 给测试提供 Redis 连接：连的是 docker compose 起的那套。
package testredis

import (
	"os"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// urlEnv 是测试用 Redis 连接串的环境变量名，没设置时连本地 compose 起的那套。
const urlEnv = "GATEWAY_TEST_REDIS_URL"

const defaultURL = "redis://localhost:6379/0"

var (
	once     sync.Once
	shared   *redis.Client
	parseErr error
)

// New 返回整个测试二进制共用的 Redis 客户端。
// 测试之间不清库：每个测试用的键都是唯一的（Key 的哈希是随机的，限流的桶按 Key 的 ID 分），
// 所以可以并行。
func New(t testing.TB) *redis.Client {
	t.Helper()
	once.Do(open)
	if parseErr != nil {
		t.Fatalf("connect to test redis (run: docker compose up -d): %v", parseErr)
	}
	if err := shared.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("ping test redis (run: docker compose up -d): %v", err)
	}
	return shared
}

// URL 返回测试用的 Redis 连接串。
func URL() string {
	if url := os.Getenv(urlEnv); url != "" {
		return url
	}
	return defaultURL
}

func open() {
	opts, err := redis.ParseURL(URL())
	if err != nil {
		parseErr = err
		return
	}
	shared = redis.NewClient(opts)
}
