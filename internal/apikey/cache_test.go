package apikey_test

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/testdb"
	"github.com/eeee0717/llm-gateway/internal/testredis"
)

// 第二次查从缓存里拿：绕过网关直接改库之后，查到的还是旧值，说明这一次没碰数据库。
func TestSecondLookupComesFromTheCache(t *testing.T) {
	db := testdb.New(t)
	cache := newCache(t, db)
	hash, keyID := newKeyInDB(t, db, 600)

	first, err := cache.IdentityByHash(t.Context(), hash)
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE api_keys SET rpm_limit = 1, disabled = TRUE WHERE id = ?`, keyID).Error)
	second, err := cache.IdentityByHash(t.Context(), hash)

	require.NoError(t, err)
	require.Equal(t, apikey.Identity{ID: keyID, RPM: 600}, first) // 缓存里只有身份和额度，没有余额
	require.Equal(t, first, second)
}

// 查不到的 Key 也记进缓存，不然拿随机 Key 反复打过来，每个请求都要查一趟数据库。
func TestLookupCachesThatAKeyDoesNotExist(t *testing.T) {
	db := testdb.New(t)
	cache := newCache(t, db)
	_, hash := apikey.Generate()

	_, err := cache.IdentityByHash(t.Context(), hash)
	require.ErrorIs(t, err, apikey.ErrNotFound)

	_, err = apikey.NewStore(db).Create(t.Context(), "created later", hash, 0)
	require.NoError(t, err)

	_, err = cache.IdentityByHash(t.Context(), hash)
	require.ErrorIs(t, err, apikey.ErrNotFound) // 代价：这个 Key 要等缓存过期才能用
}

// Redis 连不上时直接查数据库：缓存只是快一点，不是必需的，见 docs/adr/0002。
func TestLookupFallsBackToTheDatabaseWhenRedisIsDown(t *testing.T) {
	db := testdb.New(t)
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { down.Close() })
	cache := apikey.NewCache(down, apikey.NewStore(db), noJitter, slog.New(slog.DiscardHandler))
	hash, keyID := newKeyInDB(t, db, 60)

	got, err := cache.IdentityByHash(t.Context(), hash)

	require.NoError(t, err)
	require.Equal(t, apikey.Identity{ID: keyID, RPM: 60}, got)
}

func newCache(t *testing.T, db *gorm.DB) *apikey.Cache {
	t.Helper()
	return apikey.NewCache(testredis.New(t), apikey.NewStore(db), noJitter, slog.New(slog.DiscardHandler))
}

// noJitter 把 TTL 的随机偏移固定成 0，这些测试就不用管缓存什么时候过期。
func noJitter() time.Duration { return 0 }

// newKeyInDB 建一个 Key，返回它的哈希和 ID。哈希是随机的，所以每个测试的缓存键互不相干。
func newKeyInDB(t *testing.T, db *gorm.DB, rpm int) (string, int64) {
	t.Helper()
	_, hash := apikey.Generate()
	key, err := apikey.NewStore(db).Create(t.Context(), "cache test", hash, rpm)
	require.NoError(t, err)
	return hash, key.ID
}

// 击穿：一个高频 Key 的缓存过期的那一瞬间，所有在飞的请求都会发现缓存没了，
// 一起回源就是几十条一模一样的查询同时打到数据库上。同一个哈希的并发回源合并成一次。
func TestConcurrentLookupsQueryTheStoreOnce(t *testing.T) {
	const callers = 50
	store := &blockingStore{identity: apikey.Identity{ID: rand.Int64(), RPM: 60}, release: make(chan struct{})}
	cache := apikey.NewCache(testredis.New(t), store, noJitter, slog.New(slog.DiscardHandler))
	_, hash := apikey.Generate()

	got := make([]apikey.Identity, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = cache.IdentityByHash(t.Context(), hash)
		}()
	}
	// 等第一次回源真的打进数据库，再多给一会儿，让其余请求也走完各自的那次 Redis GET，
	// 撞上这次还没结束的查询。一次 GET 是零点几毫秒的事，100 毫秒是很宽的余量。
	require.Eventually(t, func() bool { return store.calls.Load() >= 1 }, time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	close(store.release)
	wg.Wait()

	require.NoError(t, errors.Join(errs...))
	require.EqualValues(t, 1, store.calls.Load())
	require.Equal(t, slices.Repeat([]apikey.Identity{store.identity}, callers), got) // 每个请求都拿到了这一次的结果
}

// 雪崩：一批同时建出来的 Key 第一次被用到的时间也挨得很近，TTL 固定的话它们会集体过期，
// 回源在同一瞬间涌向数据库。TTL 上带一个随机偏移，过期时间就摊开了。
func TestCachedKeysDoNotExpireTogether(t *testing.T) {
	const keys = 10
	db := testdb.New(t)
	rdb := testredis.New(t)
	cache := apikey.NewCache(rdb, apikey.NewStore(db), apikey.RandomJitter, slog.New(slog.DiscardHandler))

	ttls := make([]time.Duration, keys)
	for i := range keys {
		hash, _ := newKeyInDB(t, db, 60)
		_, err := cache.IdentityByHash(t.Context(), hash)
		require.NoError(t, err)
		ttl, err := rdb.PTTL(t.Context(), "auth:"+hash).Result() // 键名和 cache.go 里的 cachePrefix 对齐
		require.NoError(t, err)
		ttls[i] = ttl
	}

	// 偏移只减不增：TTL 最长还是 5 分钟，被禁用的 Key 最多陈旧这么久这句话不用改口。
	require.LessOrEqual(t, slices.Max(ttls), 5*time.Minute)
	require.Greater(t, slices.Min(ttls), 4*time.Minute+29*time.Second) // PTTL 读到的是剩下的时间，比写进去的略少
	// 摊开了：十个 TTL 至少铺开一秒。要求它们两两不同就是在赌随机数，没必要。
	require.Greater(t, slices.Max(ttls)-slices.Min(ttls), time.Second)
}

// blockingStore 冒充数据库：记下被查了几次，并把查询按在那里不返回，
// 好让后面的请求撞上同一次还没结束的回源。
type blockingStore struct {
	identity apikey.Identity
	calls    atomic.Int64
	release  chan struct{}
}

func (s *blockingStore) IdentityByHash(ctx context.Context, _ string) (apikey.Identity, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
		return s.identity, nil
	case <-ctx.Done():
		return apikey.Identity{}, ctx.Err()
	}
}
