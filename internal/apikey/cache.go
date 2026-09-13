package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// cachePrefix 是鉴权缓存的键前缀，后面拼 Key 的哈希。
const cachePrefix = "auth:"

const (
	// hitTTL 是查到的 Key 在缓存里待多久。禁用和改额度都会主动删缓存，所以它只是兜底。
	hitTTL = 5 * time.Minute
	// missTTL 是"这个 Key 不存在"记多久，短一些：它挡的是拿随机 Key 反复打过来的请求，
	// 而一个刚建出来的 Key 不该因为之前有人查过就用不了太久。
	missTTL = 30 * time.Second
	// forgetTimeout 是删缓存这一步自己的超时。它不跟着管理员的请求走，所以得有个上限。
	forgetTimeout = time.Second
)

// Identity 是鉴权要用的 Key 信息：身份和限流额度。余额不在里面——余额只有 PostgreSQL 一份，
// 缓存里的东西丢了都能重建，见 docs/adr/0002。
type Identity struct {
	ID       int64 `json:"id"`
	Disabled bool  `json:"disabled"`
	RPM      int   `json:"rpm"` // 每分钟额度，0 表示用配置里的默认值
}

// Cache 是 Store 的鉴权缓存，Cache-Aside：先查 Redis，没有就查数据库再写回。
// Redis 出任何问题都只记一条日志，然后当缓存不存在处理。
type Cache struct {
	rdb    *redis.Client
	store  *Store
	logger *slog.Logger
}

func NewCache(rdb *redis.Client, store *Store, logger *slog.Logger) *Cache {
	return &Cache{rdb: rdb, store: store, logger: logger}
}

// IdentityByHash 按 Key 的哈希查出鉴权要用的信息，缓存没有就回源数据库并写回。
func (c *Cache) IdentityByHash(ctx context.Context, hash string) (Identity, error) {
	if cached, ok := c.get(ctx, hash); ok {
		if cached.ID == 0 {
			return Identity{}, ErrNotFound // 记下来的"这个 Key 不存在"
		}
		return cached, nil
	}
	identity, err := c.store.IdentityByHash(ctx, hash)
	switch {
	case errors.Is(err, ErrNotFound):
		c.put(ctx, hash, Identity{}, missTTL)
		return Identity{}, ErrNotFound
	case err != nil:
		return Identity{}, err
	}
	c.put(ctx, hash, identity, hitTTL)
	return identity, nil
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
