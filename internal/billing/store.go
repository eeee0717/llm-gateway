package billing

import (
	"context"

	"gorm.io/gorm"

	"github.com/eeee0717/llm-gateway/internal/openai"
)

// store 读写余额和用量记录。
type store struct {
	db *gorm.DB
}

func newStore(db *gorm.DB) *store {
	return &store{db: db}
}

// record 是一次结算要写下的内容。
type record struct {
	requestID     string
	keyID         int64
	model         string
	usage         openai.Usage
	costMicro     int64
	reservedMicro int64
	estimated     bool
}

// reserve 从余额里扣下 amount，返回是否扣成功。
//
// 余额判断和扣减在同一条 UPDATE 里完成，没有"先查再改"的空档：同一个 Key 的并发预扣由这一行的行锁排队，
// 后到的看到的已经是扣过的余额，所以多个网关实例一起跑也不会把余额扣穿。被禁用的 Key 也在这里挡掉。
func (s *store) reserve(ctx context.Context, keyID, amount int64) (bool, error) {
	res := s.db.WithContext(ctx).Exec(`
		UPDATE api_keys SET balance_micro = balance_micro - ?
		WHERE id = ? AND NOT disabled AND balance_micro >= ?`, amount, keyID, amount)
	return res.RowsAffected == 1, res.Error
}

// settle 把预扣和实际费用的差额退回余额，并写一条用量记录，两件事在同一个事务里。
// 用量记录以请求 ID 为主键：重复结算时它插不进去，余额也就不会再动一次。
func (s *store) settle(ctx context.Context, r record) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`
			INSERT INTO usage_records (request_id, api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (request_id) DO NOTHING`,
			r.requestID, r.keyID, r.model, r.usage.PromptTokens, r.usage.CompletionTokens, r.costMicro, r.estimated)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil // 这个请求已经结算过
		}
		return tx.Exec(`UPDATE api_keys SET balance_micro = balance_micro + ? WHERE id = ?`,
			r.reservedMicro-r.costMicro, r.keyID).Error
	})
}
