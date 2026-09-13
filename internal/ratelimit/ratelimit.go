// Package ratelimit 按 API Key 限流：令牌桶存在 Redis 里，多个网关实例共享同一个桶。
// 桶容量等于每分钟的额度，补充速率是额度除以 60 秒，所以最多能攒下一分钟的突发。
// 规则见 docs/adr/0004，Redis 不可用时放行见 docs/adr/0002。
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix 是桶在 Redis 里的键前缀，后面拼 API Key 的 ID。
const keyPrefix = "ratelimit:"

// idleTTL 是桶多久没人用就删掉。比装满一个桶的时间长，删掉的一定是已经满了的桶，
// 下次再来时按"桶是满的"重新开始，和留着它是一回事。
const idleTTL = 5 * time.Minute

// bucket 在一次 EVAL 里完成补充和扣减。分成"读-算-写"三步的话，
// 两个实例可能读到同一个令牌数，各自扣一个，桶就被多用了一次。
//
// KEYS[1] 桶的键；ARGV 依次是容量、当前时间（毫秒）、空闲多久删掉（毫秒）。
// 返回两个数：能不能放行，以及被拒时还要等多少毫秒。
var bucket = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local now      = tonumber(ARGV[2])
local ttl      = tonumber(ARGV[3])

local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])
if tokens == nil then      -- 没见过这个桶，按满的算
  tokens = capacity
  ts = now
end

-- 按经过的时间补充。实例之间的时钟不完全一致，所以经过的时间取不小于零的那部分。
local elapsed = math.max(0, now - ts)
tokens = math.min(capacity, tokens + elapsed * capacity / 60000)

local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) * 60000 / capacity)
end

-- 记下的时间只进不退。让落后的实例把它拨回去的话，超前的实例下一次就会重新算出一大段
-- "经过的时间"再补一次，而且每次交替都补，桶等于没有上限。
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', math.max(ts, now))
redis.call('PEXPIRE', KEYS[1], ttl)
return {allowed, retry}
`)

// Limiter 按 API Key 限流。当前时间由外面传进来，测试才能让"放行了多少个"是个确定的数，
// 见 docs/adr/0004。
type Limiter struct {
	rdb    *redis.Client
	now    func() time.Time
	logger *slog.Logger
}

func New(rdb *redis.Client, now func() time.Time, logger *slog.Logger) *Limiter {
	return &Limiter{rdb: rdb, now: now, logger: logger}
}

// Allow 为这个 Key 取走一个令牌。取不到时返回还要等多久。
// Redis 出任何问题都放行：限流是保护上游的，拒绝请求的代价比放过几个大。
func (l *Limiter) Allow(ctx context.Context, keyID int64, rpm int) (bool, time.Duration) {
	key := keyPrefix + fmt.Sprint(keyID)
	res, err := bucket.Run(ctx, l.rdb, []string{key},
		rpm, l.now().UnixMilli(), idleTTL.Milliseconds()).Int64Slice()
	if err != nil {
		l.logger.WarnContext(ctx, "rate limit check failed, letting the request through", "error", err)
		return true, 0
	}
	if res[0] == 1 {
		return true, 0
	}
	return false, time.Duration(res[1]) * time.Millisecond
}

// RetryAfterSeconds 把等待时间折成 Retry-After 头要的秒数，不足一秒按一秒算。
func RetryAfterSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}
