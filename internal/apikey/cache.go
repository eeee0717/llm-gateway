package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// cachePrefix 是鉴权缓存的键前缀，后面拼 Key 的哈希。
const cachePrefix = "auth:"

const (
	// hitTTL 是查到的 Key 在缓存里待多久。禁用和改额度都会主动删缓存，所以它只是兜底。
	hitTTL = 5 * time.Minute
	// maxJitter 是写缓存时从 hitTTL 上随机减掉的最大值，取 hitTTL 的十分之一，即 30 秒。
	//
	// 一批 Key 常常是一起建出来、一起开始被调用的，TTL 是个定数的话它们就在同一刻集体过期，
	// 回源一起涌向数据库。偏移把过期时间摊到 30 秒里，一千个 Key 也就是每秒三十几次回源。
	// 偏移只减不增：缓存最长还是 hitTTL，"删缓存失败时最多陈旧 5 分钟"这句话不用改口。
	// 再摊得开一些也行，代价是平均 TTL 变短、白白多回源几次，十分之一是这两头之间的常见取法。
	maxJitter = hitTTL / 10
	// missTTL 是"这个 Key 不存在"记多久，短一些：它挡的是拿随机 Key 反复打过来的请求，
	// 而一个刚建出来的 Key 不该因为之前有人查过就用不了太久。
	// 它不加偏移：这些键本来就是零散写进去的，30 秒后也凑不到一块儿过期。
	missTTL = 30 * time.Second
	// forgetTimeout 是删缓存这一步自己的超时。它不跟着管理员的请求走，所以得有个上限。
	forgetTimeout = time.Second
	// loadTimeout 是一次回源自己的超时。回源同样不跟着某一个调用方的请求走，见 load。
	// 一次走唯一索引的等值查询是零点零几毫秒的事，到得了这个数就说明数据库已经出问题了。
	loadTimeout = 3 * time.Second
)

// RandomJitter 返回 [0, maxJitter) 里的一个偏移量，写缓存时从 hitTTL 上减掉。
// 生产用它；测试注入一个固定值，TTL 才是个确定的数，和 ratelimit 注入时钟是一个道理。
func RandomJitter() time.Duration {
	return time.Duration(rand.Int64N(int64(maxJitter)))
}

// Identity 是鉴权要用的 Key 信息：身份和限流额度。余额不在里面——余额只有 MySQL 一份，
// 缓存里的东西丢了都能重建，见 docs/adr/0002。
type Identity struct {
	ID       int64 `json:"id"`
	Disabled bool  `json:"disabled"`
	RPM      int   `json:"rpm"` // 每分钟额度，0 表示用配置里的默认值
}

// Cache 是 Lookup 的鉴权缓存，Cache-Aside：先查 Redis，没有就查数据库再写回。
// Redis 出任何问题都只记一条日志，然后当缓存不存在处理。
type Cache struct {
	rdb    *redis.Client
	store  Lookup
	jitter func() time.Duration
	logger *slog.Logger
	// group 把同一个哈希的并发回源合并成一次，见 load。
	group singleflight.Group
}

func NewCache(rdb *redis.Client, store Lookup, jitter func() time.Duration, logger *slog.Logger) *Cache {
	return &Cache{rdb: rdb, store: store, jitter: jitter, logger: logger}
}

// IdentityByHash 按 Key 的哈希查出鉴权要用的信息，缓存没有就回源数据库并写回。
func (c *Cache) IdentityByHash(ctx context.Context, hash string) (Identity, error) {
	if cached, ok := c.get(ctx, hash); ok {
		if cached.ID == 0 {
			return Identity{}, ErrNotFound // 记下来的"这个 Key 不存在"
		}
		return cached, nil
	}
	return c.load(ctx, hash)
}

// load 回源数据库并把结果写回缓存。同一个哈希的并发回源合并成一次：一个高频 Key 的缓存过期的
// 那一瞬间，所有在飞的请求都会发现缓存没了，没有这一层就是几百条一模一样的查询同时压到数据库上。
//
// 合并出来的那次查询不跟着任何一个调用方的请求取消：结果是所有在等的请求共用的，先到的那个断开连接，
// 不该把其他人的查询一起取消。脱开之后它自己要有个上限，和 Forget 是一个道理。
// 每个请求仍然只等到自己的 context 结束为止，不会被一次慢查询拖住。
func (c *Cache) load(ctx context.Context, hash string) (Identity, error) {
	shared := c.group.DoChan(hash, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		identity, err := c.store.IdentityByHash(ctx, hash)
		switch {
		case errors.Is(err, ErrNotFound):
			c.put(ctx, hash, Identity{}, missTTL)
		case err == nil:
			c.put(ctx, hash, identity, hitTTL-c.jitter())
		}
		return identity, err
	})
	select {
	case res := <-shared:
		identity, _ := res.Val.(Identity)
		return identity, res.Err
	case <-ctx.Done():
		return Identity{}, ctx.Err()
	}
}

// Forget 删掉一个 Key 的缓存。禁用或者改额度之后调用，让改动立刻生效，不用等 TTL。
//
// 这一步不跟着管理员的请求取消：数据库已经改完了，管理员这时断开连接的话，缓存就留着不删，
// 被禁用的 Key 还能再用一个 TTL。和结算的处理是一回事，见 internal/relay。
// 删不掉时只能记日志：改动仍然会在 TTL 到期后生效，而管理员那边的操作确实成功了。
func (c *Cache) Forget(ctx context.Context, hash string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetTimeout)
	defer cancel()
	if err := c.rdb.Del(ctx, cachePrefix+hash).Err(); err != nil {
		c.logger.ErrorContext(ctx, "auth cache delete failed", "error", err, "ttl", hitTTL)
	}
}

func (c *Cache) get(ctx context.Context, hash string) (Identity, bool) {
	data, err := c.rdb.Get(ctx, cachePrefix+hash).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return Identity{}, false // 没缓存，正常情况
	case err != nil:
		c.logger.WarnContext(ctx, "auth cache read failed", "error", err)
		return Identity{}, false
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		c.logger.WarnContext(ctx, "auth cache holds a bad value", "error", err)
		return Identity{}, false
	}
	return identity, true
}

func (c *Cache) put(ctx context.Context, hash string, identity Identity, ttl time.Duration) {
	data, err := json.Marshal(identity)
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, cachePrefix+hash, data, ttl).Err(); err != nil {
		c.logger.WarnContext(ctx, "auth cache write failed", "error", err)
	}
}
