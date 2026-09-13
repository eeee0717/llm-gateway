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
	rdb := testredis.New(t)
	logger := slog.New(slog.DiscardHandler)
	ahead := &clock{now: time.Now()}
	behind := &clock{now: ahead.now.Add(-time.Minute)}
	fast := ratelimit.New(rdb, ahead.Now, logger)
	slow := ratelimit.New(rdb, behind.Now, logger)
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
