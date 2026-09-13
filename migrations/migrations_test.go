package migrations_test

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/testdb"
	"github.com/eeee0717/llm-gateway/migrations"
)

func TestSchemaStoresKeysAndUsageRecords(t *testing.T) {
	db := testdb.New(t)

	var key struct {
		ID           int64
		BalanceMicro int64
		Disabled     bool
		CreatedAt    time.Time
	}
	require.NoError(t, db.Raw(
		`INSERT INTO api_keys (name, key_hash) VALUES (?, ?) RETURNING id, balance_micro, disabled, created_at`,
		"test key", rand.Text(),
	).Scan(&key).Error)

	require.NotZero(t, key.ID)
	require.Zero(t, key.BalanceMicro) // 新建的 Key 余额为零，充值后才能用
	require.False(t, key.Disabled)
	require.WithinDuration(t, time.Now(), key.CreatedAt, time.Minute)

	requestID := rand.Text()
	require.NoError(t, db.Exec(
		`INSERT INTO usage_records (request_id, api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		requestID, key.ID, "mock-model", 5, 3, 1200, false,
	).Error)

	var record struct {
		PromptTokens     int
		CompletionTokens int
		CostMicro        int64
		Estimated        bool
	}
	require.NoError(t, db.Raw(
		`SELECT prompt_tokens, completion_tokens, cost_micro, estimated FROM usage_records WHERE request_id = ?`,
		requestID,
	).Scan(&record).Error)
	require.Equal(t, 5, record.PromptTokens)
	require.Equal(t, 3, record.CompletionTokens)
	require.EqualValues(t, 1200, record.CostMicro)
	require.False(t, record.Estimated)
}

// 鉴权是按哈希的等值查询，同一个哈希只能对应一个 Key。
func TestKeyHashIsUnique(t *testing.T) {
	db := testdb.New(t)
	hash := rand.Text()

	require.NoError(t, insertKey(db, hash))
	require.Error(t, insertKey(db, hash))
}

// 结算的幂等键：一个请求至多一条用量记录。
func TestUsageRecordIsUniquePerRequest(t *testing.T) {
	db := testdb.New(t)
	hash := rand.Text()
	require.NoError(t, insertKey(db, hash))
	var keyID int64
	require.NoError(t, db.Raw(`SELECT id FROM api_keys WHERE key_hash = ?`, hash).Scan(&keyID).Error)
	requestID := rand.Text()

	require.NoError(t, insertUsageRecord(db, requestID, keyID))
	require.Error(t, insertUsageRecord(db, requestID, keyID))
}

// 已经迁移过的库再执行一次迁移不会出错：两个实例同时启动、或者重启后再跑都是这种情况。
func TestUpIsRepeatable(t *testing.T) {
	db, err := testdb.New(t).DB()
	require.NoError(t, err)

	require.NoError(t, migrations.Up(db))
}

func insertKey(db *gorm.DB, hash string) error {
	return db.Exec(`INSERT INTO api_keys (name, key_hash) VALUES (?, ?)`, "test key", hash).Error
}

func insertUsageRecord(db *gorm.DB, requestID string, keyID int64) error {
	return db.Exec(
		`INSERT INTO usage_records (request_id, api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		requestID, keyID, "mock-model", 5, 3, 1200, true,
	).Error
}
