package billing

import (
	"context"
	"errors"

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
//
// 扣没扣下看 RowsAffected。连接串里开了 clientFoundRows，它数的才是"WHERE 匹配到几行"；
// 按 MySQL 默认的"改动了几行"算，免费模型预扣 0 微元时这条 UPDATE 什么也没改，会被当成余额不够。
func (s *store) reserve(ctx context.Context, keyID, amount int64) (bool, error) {
	res := s.db.WithContext(ctx).Exec(`
		UPDATE api_keys SET balance_micro = balance_micro - ?
		WHERE id = ? AND NOT disabled AND balance_micro >= ?`, amount, keyID, amount)
	return res.RowsAffected == 1, res.Error
}

// errAlreadySettled 只在事务内部传递：让这次结算整个回滚掉，对外仍然算成功——这个请求之前就结算过了。
var errAlreadySettled = errors.New("already settled")

// settle 把预扣和实际费用的差额退回余额，并写一条用量记录，两件事在同一个事务里。
//
// 两句的先后是定死的：先 UPDATE 余额，再 INSERT 用量记录。反过来写会死锁——usage_records 上有
// 指向 api_keys 的外键，InnoDB 校验它时会在父行上加共享锁，紧接着的 UPDATE 又要把同一行升级成排他锁，
// 同一个 Key 的并发结算于是各拿着共享锁等对方放手（Error 1213）。实测 1000 个并发请求里有 2% 撞上。
// 先取排他锁就没有"升级"这一步，也就构不成环。PostgreSQL 上碰不到：它的外键加的是 FOR KEY SHARE，
// 只和改动键列的语句冲突，而这里改的是余额。
//
// 用量记录以请求 ID 为主键，重复结算时这条 INSERT 会撞主键：整个事务回滚，刚退的那笔钱跟着撤销，
// 余额只被退一次。MySQL 没有 ON CONFLICT DO NOTHING，"已经结算过"就是靠这个冲突认出来的；
// 认错误交给 GORM 的 TranslateError，业务代码不用去认 MySQL 的 1062，见 cmd/gateway 里开库的地方。
func (s *store) settle(ctx context.Context, r record) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE api_keys SET balance_micro = balance_micro + ? WHERE id = ?`,
			r.reservedMicro-r.costMicro, r.keyID).Error; err != nil {
			return err
		}
		err := tx.Exec(`
			INSERT INTO usage_records (request_id, api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.requestID, r.keyID, r.model, r.usage.PromptTokens, r.usage.CompletionTokens, r.costMicro, r.estimated).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return errAlreadySettled
		}
		return err
	})
	if errors.Is(err, errAlreadySettled) {
		return nil
	}
	return err
}
