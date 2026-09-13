package apikey_test

import (
	"log/slog"
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
	cache := apikey.NewCache(down, apikey.NewStore(db), slog.New(slog.DiscardHandler))
	hash, keyID := newKeyInDB(t, db, 60)

	got, err := cache.IdentityByHash(t.Context(), hash)

	require.NoError(t, err)
	require.Equal(t, apikey.Identity{ID: keyID, RPM: 60}, got)
}

func newCache(t *testing.T, db *gorm.DB) *apikey.Cache {
	t.Helper()
	return apikey.NewCache(testredis.New(t), apikey.NewStore(db), slog.New(slog.DiscardHandler))
}

// newKeyInDB 建一个 Key，返回它的哈希和 ID。哈希是随机的，所以每个测试的缓存键互不相干。
func newKeyInDB(t *testing.T, db *gorm.DB, rpm int) (string, int64) {
	t.Helper()
	_, hash := apikey.Generate()
	key, err := apikey.NewStore(db).Create(t.Context(), "cache test", hash, rpm)
	require.NoError(t, err)
	return hash, key.ID
}
