package ratelimit_test

import (
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/ratelimit"
	"github.com/eeee0717/llm-gateway/internal/testredis"
)

// rpm 是测试里用的额度：每分钟 60 个，正好一秒补一个令牌。
const rpm = 60

// M3 收口断言：放行的请求总数正确。桶一开始是满的，瞬间打过来的请求正好放行 rpm 个。
// 时间是注入的，所以这个数字是确定的，不受机器快慢影响，见 docs/adr/0004。
func TestBucketStartsFullAndAllowsExactlyTheQuota(t *testing.T) {
	l, _ := newLimiter(t)
	keyID := rand.Int64()

	allowed := 0
	for range rpm * 3 {
		if ok, _ := l.Allow(t.Context(), keyID, rpm); ok {
			allowed++
		}
	}

	require.Equal(t, rpm, allowed)
}

// 两个实例同时打同一个 Key：补充和扣减在 Redis 里一次做完，所以放行的总数仍然正好是额度。
// 拆成"读令牌数-算-写回"的话，两个实例会读到同一个数各扣一次，桶就被多用了。
func TestConcurrentCallersShareOneBucket(t *testing.T) {
	c := &clock{now: time.Now()} // 两个实例共用一个时钟，期间不往前走，没有令牌补充进来
	first := ratelimit.New(testredis.New(t), c.Now, slog.New(slog.DiscardHandler))
	second := ratelimit.New(testredis.New(t), c.Now, slog.New(slog.DiscardHandler))
	keyID := rand.Int64()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range rpm * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := first
			if i%2 == 1 {
				l = second
			}
			if ok, _ := l.Allow(t.Context(), keyID, rpm); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	require.EqualValues(t, rpm, allowed.Load())
}

// 时间往前走，令牌按额度补回来：额度 60 就是一秒一个。
func TestTokensRefillOverTime(t *testing.T) {
	l, clock := newLimiter(t)
	keyID := rand.Int64()
	drain(t, l, keyID)

	clock.advance(30 * time.Second)

	allowed := 0
	for range rpm {
		if ok, _ := l.Allow(t.Context(), keyID, rpm); ok {
			allowed++
		}
	}
	require.Equal(t, 30, allowed)
}

// 实例之间的时钟不可能完全一致。落后的那个来过之后，不能把桶上记的时间往回拨——
// 否则超前的实例下一次调用会重新算出一大段"经过的时间"，把桶补满，而且每次交替都补一次，
// 限流就形同虚设。
func TestALaggingClockDoesNotRefillTheBucket(t *testing.T) {
	fast, slow := twoClocks(t)
	keyID := rand.Int64()
	drain(t, fast, keyID)

	ok, _ := slow.Allow(t.Context(), keyID, rpm) // 桶是空的，落后的实例同样被拒
	require.False(t, ok)

	allowed := 0
	for range rpm {
		if ok, _ := fast.Allow(t.Context(), keyID, rpm); ok {
			allowed++
		}
	}
	require.Zero(t, allowed) // 时间没往前走过，一个令牌也不该补回来
}

// 时钟往回跳也不会把桶抽干：经过的时间取不小于零的那部分，否则"负的经过时间"
// 会算出负的补充量，把桶里剩下的令牌抹掉。
func TestALaggingClockDoesNotDrainTheBucket(t *testing.T) {
	fast, slow := twoClocks(t)
	keyID := rand.Int64()
	ok, _ := fast.Allow(t.Context(), keyID, rpm) // 建出桶，用掉一个令牌
	require.True(t, ok)

	ok, _ = slow.Allow(t.Context(), keyID, rpm)

	require.True(t, ok) // 桶里还有 59 个
}

// 补充不会超过桶的容量：停一整天回来，也只有一分钟的量。
func TestRefillStopsAtCapacity(t *testing.T) {
	l, clock := newLimiter(t)
	keyID := rand.Int64()
	drain(t, l, keyID)

	clock.advance(24 * time.Hour)

	allowed := 0
	for range rpm * 2 {
		if ok, _ := l.Allow(t.Context(), keyID, rpm); ok {
			allowed++
		}
	}
	require.Equal(t, rpm, allowed)
}

// 额度不是正数时不限流。脚本按容量算补充速率：容量为 0 会算出无穷大的等待时间，
// 传到 Go 这边溢出成负数，调用方收到 429 加一个 Retry-After: 0，会立刻重试，空转成热循环。
func TestAllowPassesWhenTheQuotaIsNotPositive(t *testing.T) {
	l, _ := newLimiter(t)

	for _, quota := range []int{0, -5} {
		ok, retryAfter := l.Allow(t.Context(), rand.Int64(), quota)
		require.True(t, ok, "额度 %d", quota)
		require.Zero(t, retryAfter, "额度 %d", quota)
	}
}

// 被拒的请求要知道等多久：额度 60 时下一个令牌在一秒后。
func TestRetryAfterIsTheWaitForTheNextToken(t *testing.T) {
	l, _ := newLimiter(t)
	keyID := rand.Int64()
	drain(t, l, keyID)

	ok, retryAfter := l.Allow(t.Context(), keyID, rpm)

	require.False(t, ok)
	require.Equal(t, time.Second, retryAfter)
}

// Redis 连不上时放行：限流是保护上游的，不是记账的，见 docs/adr/0002。
func TestAllowsWhenRedisIsDown(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { down.Close() })
	l := ratelimit.New(down, time.Now, slog.New(slog.DiscardHandler))

	ok, retryAfter := l.Allow(t.Context(), rand.Int64(), rpm)

	require.True(t, ok)
	require.Zero(t, retryAfter)
}

// twoClocks 起两个共用一套 Redis 的 Limiter，它们的时钟相差一分钟，用来模拟实例之间的时钟漂移。
func twoClocks(t *testing.T) (fast, slow *ratelimit.Limiter) {
	t.Helper()
	rdb, logger := testredis.New(t), slog.New(slog.DiscardHandler)
	now := time.Now()
	return ratelimit.New(rdb, (&clock{now: now}).Now, logger),
		ratelimit.New(rdb, (&clock{now: now.Add(-time.Minute)}).Now, logger)
}

func newLimiter(t *testing.T) (*ratelimit.Limiter, *clock) {
	t.Helper()
	c := &clock{now: time.Now()}
	return ratelimit.New(testredis.New(t), c.Now, slog.New(slog.DiscardHandler)), c
}

// drain 把桶里的令牌用光。
func drain(t *testing.T, l *ratelimit.Limiter, keyID int64) {
	t.Helper()
	for range rpm {
		ok, _ := l.Allow(t.Context(), keyID, rpm)
		require.True(t, ok)
	}
}

// clock 是测试用的时钟，时间只在 advance 时往前走。
type clock struct {
	now time.Time
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
