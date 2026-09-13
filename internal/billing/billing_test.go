package billing_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/apikey"
	"github.com/eeee0717/llm-gateway/internal/billing"
	"github.com/eeee0717/llm-gateway/internal/openai"
	"github.com/eeee0717/llm-gateway/internal/testdb"
)

// mock-model 的单价：输入 2、输出 3 元每百万 token，正好等于 2 微元和 3 微元每 token。
var prices = map[string]billing.Price{"mock-model": billing.PricePerMillionTokens(2, 3)}

// reservation 是测试里反复用的一次预扣：估算 100 个 prompt token，输出上限 1000，最大费用 3200 微元。
var reservation = billing.Reservation{Model: "mock-model", PromptTokens: 100, MaxOutputTokens: 1000}

func TestReserveTakesTheMaximumPossibleCost(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 10_000)

	reserved, ok, err := svc.Reserve(ctx, reservation)

	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 100*2+1000*3, reserved)
	require.EqualValues(t, 10_000-3_200, balanceOf(t, db, keyID))
}

func TestReserveRejectsWhenBalanceIsNotEnough(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 3_199) // 差 1 微元

	_, ok, err := svc.Reserve(ctx, reservation)

	require.NoError(t, err)
	require.False(t, ok)
	require.EqualValues(t, 3_199, balanceOf(t, db, keyID)) // 拒绝时余额不动
}

func TestReserveRejectsDisabledKey(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 10_000)
	require.NoError(t, db.Exec(`UPDATE api_keys SET disabled = TRUE WHERE id = ?`, keyID).Error)

	_, ok, err := svc.Reserve(ctx, reservation)

	require.NoError(t, err)
	require.False(t, ok)
	require.EqualValues(t, 10_000, balanceOf(t, db, keyID))
}

// 单价常有小数，费用要按整数算：0.14 元每百万 token 就是每 token 0.14 微元，
// 50 个 token 正好 7 微元，用 float64 相乘会得到 7.000000000000001，向上取整就多收 1 微元。
func TestCostIsExactWhenPriceHasDecimals(t *testing.T) {
	db := testdb.New(t)
	svc := billing.New(db, map[string]billing.Price{"cheap": billing.PricePerMillionTokens(0.14, 0.28)})
	ctx, _ := keyWithBalance(t, db, 10_000)

	reserved, ok, err := svc.Reserve(ctx, billing.Reservation{Model: "cheap", PromptTokens: 50, MaxOutputTokens: 50})

	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 21, reserved) // 50×0.14 + 50×0.28，浮点算出来是 22
}

// 请求 ID 是结算的幂等键。空的话所有请求会撞在同一个主键上，只有第一个能结算，
// 其余的预扣都退不回来，所以宁可报错。
func TestSettleRejectsAnEmptyRequestID(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 10_000)
	reserved, _, err := svc.Reserve(ctx, reservation)
	require.NoError(t, err)

	err = svc.Settle(ctx, billing.Result{Model: "mock-model", ReservedMicro: reserved})

	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprint(keyID)) // 对账要知道是哪个 Key 的钱挂住了
}

func TestSettleRefundsTheDifferenceAndRecordsUsage(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 10_000)
	reserved, _, err := svc.Reserve(ctx, reservation)
	require.NoError(t, err)
	requestID := rand.Text()

	err = svc.Settle(ctx, billing.Result{
		RequestID:     requestID,
		Model:         "mock-model",
		Usage:         openai.Usage{PromptTokens: 100, CompletionTokens: 10},
		Estimated:     true,
		ReservedMicro: reserved,
	})

	require.NoError(t, err)
	// 实际费用 100*2 + 10*3 = 230 微元，预扣的其余部分退回
	require.EqualValues(t, 10_000-230, balanceOf(t, db, keyID))
	record := recordOf(t, db, requestID)
	require.Equal(t, keyID, record.APIKeyID)
	require.Equal(t, "mock-model", record.Model)
	require.Equal(t, 100, record.PromptTokens)
	require.Equal(t, 10, record.CompletionTokens)
	require.EqualValues(t, 230, record.CostMicro)
	require.True(t, record.Estimated)
}

// 结算以请求 ID 为幂等键：重复结算不会重复扣钱，也不会多出一条用量记录。
func TestSettleTwiceChargesOnce(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 10_000)
	reserved, _, err := svc.Reserve(ctx, reservation)
	require.NoError(t, err)
	result := billing.Result{
		RequestID:     rand.Text(),
		Model:         "mock-model",
		Usage:         openai.Usage{PromptTokens: 100, CompletionTokens: 10},
		ReservedMicro: reserved,
	}

	require.NoError(t, svc.Settle(ctx, result))
	require.NoError(t, svc.Settle(ctx, result))

	require.EqualValues(t, 10_000-230, balanceOf(t, db, keyID))
	require.EqualValues(t, 1, countRecords(t, db, result.RequestID))
}

// 实际费用超过预扣时照实扣，余额可以为负；这个 Key 的下一次预扣自然会失败。
func TestSettleChargesMoreThanReservedWhenPromptWasUnderestimated(t *testing.T) {
	svc, db := newService(t)
	ctx, keyID := keyWithBalance(t, db, 3_300)
	reserved, ok, err := svc.Reserve(ctx, billing.Reservation{Model: "mock-model", PromptTokens: 0, MaxOutputTokens: 1000})
	require.NoError(t, err)
	require.True(t, ok)

	err = svc.Settle(ctx, billing.Result{
		RequestID:     rand.Text(),
		Model:         "mock-model",
		Usage:         openai.Usage{PromptTokens: 1000, CompletionTokens: 1000},
		ReservedMicro: reserved,
	})

	require.NoError(t, err)
	// 3300 - 3000（预扣）+ 3000（退回）- 5000（实际）
	require.EqualValues(t, -1_700, balanceOf(t, db, keyID))
}

// 结算是同步做的，它的耗时直接加在每个请求的延迟上，所以单独量一下。
func BenchmarkReserveAndSettle(b *testing.B) {
	db := testdb.New(b)
	svc := billing.New(db, prices)
	_, hash := apikey.Generate()
	key, err := apikey.NewStore(db).Create(b.Context(), "benchmark", hash, 0)
	require.NoError(b, err)
	require.NoError(b, db.Exec(`UPDATE api_keys SET balance_micro = ? WHERE id = ?`, int64(1)<<40, key.ID).Error)
	ctx := apikey.NewContext(b.Context(), apikey.Identity{ID: key.ID})

	b.Run("reserve", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := svc.Reserve(ctx, reservation); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("settle", func(b *testing.B) {
		for b.Loop() {
			err := svc.Settle(ctx, billing.Result{
				RequestID:     rand.Text(),
				Model:         "mock-model",
				Usage:         openai.Usage{PromptTokens: 1, CompletionTokens: 5},
				ReservedMicro: 42,
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func newService(t *testing.T) (*billing.Service, *gorm.DB) {
	t.Helper()
	db := testdb.New(t)
	return billing.New(db, prices), db
}

// keyWithBalance 建一个有余额的 Key，返回带着这个 Key 的 context，计费从 context 里认出扣谁的钱。
func keyWithBalance(t *testing.T, db *gorm.DB, balance int64) (context.Context, int64) {
	t.Helper()
	_, hash := apikey.Generate()
	key, err := apikey.NewStore(db).Create(t.Context(), "test key", hash, 0)
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE api_keys SET balance_micro = ? WHERE id = ?`, balance, key.ID).Error)
	return apikey.NewContext(t.Context(), apikey.Identity{ID: key.ID}), key.ID
}

// balanceOf 查一个 Key 的余额。查不到行时 Scan 不报错、balance 留在零值，所以这里要看行数：
// 否则"Key 被删了"和"余额是 0"分不开。
func balanceOf(t *testing.T, db *gorm.DB, keyID int64) int64 {
	t.Helper()
	var balance int64
	res := db.Raw(`SELECT balance_micro FROM api_keys WHERE id = ?`, keyID).Scan(&balance)
	require.NoError(t, res.Error)
	require.EqualValues(t, 1, res.RowsAffected, "api key %d not found", keyID)
	return balance
}

type usageRecord struct {
	APIKeyID         int64
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostMicro        int64
	Estimated        bool
}

func recordOf(t *testing.T, db *gorm.DB, requestID string) usageRecord {
	t.Helper()
	var record usageRecord
	require.NoError(t, db.Raw(
		`SELECT api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated
		 FROM usage_records WHERE request_id = ?`, requestID).Scan(&record).Error)
	return record
}

func countRecords(t *testing.T, db *gorm.DB, requestID string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM usage_records WHERE request_id = ?`, requestID).Scan(&count).Error)
	return count
}
