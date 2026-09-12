package apikey_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/testdb"
)

func TestGenerateReturnsPlainKeyAndItsHash(t *testing.T) {
	plain, hash := apikey.Generate()
	other, _ := apikey.Generate()

	require.True(t, strings.HasPrefix(plain, "sk-"))
	require.NotEqual(t, plain, other) // 每次生成的 Key 都不同
	require.Equal(t, hash, apikey.Hash(plain))
	require.NotContains(t, hash, plain) // 哈希里不含明文
	require.Len(t, hash, 64)            // SHA-256 的十六进制
}

func TestCreateStoresHashNotPlainKey(t *testing.T) {
	db := testdb.New(t)
	store := apikey.NewStore(db)
	plain, hash := apikey.Generate()

	key, err := store.Create(t.Context(), "alice", hash)

	require.NoError(t, err)
	require.NotZero(t, key.ID)
	require.Equal(t, "alice", key.Name)
	require.Zero(t, key.BalanceMicro) // 新建的 Key 余额为零，充值后才能用
	require.False(t, key.Disabled)

	var stored string
	require.NoError(t, db.Raw(`SELECT key_hash FROM api_keys WHERE id = ?`, key.ID).Scan(&stored).Error)
	require.Equal(t, hash, stored)
	require.NotEqual(t, plain, stored) // 明文不落库，丢了只能重新创建
}
