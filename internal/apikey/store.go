package apikey

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrNotFound 表示库里没有这个 Key。GORM 的错误不往外传，调用方只认这一个。
var ErrNotFound = errors.New("api key not found")

// Key 是一个 API Key 的记录。余额的单位是微元（1 微元 = 0.000001 元），见 docs/adr/0001。
type Key struct {
	ID           int64
	Name         string
	KeyHash      string
	BalanceMicro int64
	Disabled     bool
	RPMLimit     int // 每分钟允许的请求数，0 表示用配置里的默认值，见 docs/adr/0004
	CreatedAt    time.Time
}

// TableName 指定表名，否则 GORM 会按类型名推成 keys。
func (Key) TableName() string { return "api_keys" }

// Store 读写 api_keys 表。余额的预扣与结算不在这里，它们要和用量记录在同一个事务里，见 internal/billing。
type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Create 新建一个 Key。余额为零，管理员充值后才能用；rpmLimit 为 0 表示用配置里的默认额度。
func (s *Store) Create(ctx context.Context, name, hash string, rpmLimit int) (Key, error) {
	key := Key{Name: name, KeyHash: hash, RPMLimit: rpmLimit}
	err := s.db.WithContext(ctx).Create(&key).Error
	return key, err
}

// ByHash 按 Key 的哈希查记录。鉴权走这条路径，命中的是 key_hash 上的唯一索引。
func (s *Store) ByHash(ctx context.Context, hash string) (Key, error) {
	return s.find(ctx, "key_hash = ?", hash)
}

// ByID 按 ID 查记录，管理接口用。
func (s *Store) ByID(ctx context.Context, id int64) (Key, error) {
	return s.find(ctx, "id = ?", id)
}

// Credit 给余额加上 amountMicro，返回更新后的记录。充值是累加，不是设定值。
func (s *Store) Credit(ctx context.Context, id, amountMicro int64) (Key, error) {
	return s.update(ctx, `UPDATE api_keys SET balance_micro = balance_micro + ? WHERE id = ? RETURNING *`, amountMicro, id)
}

// SetRPMLimit 改一个 Key 的限流额度。传 0 表示改回"跟着配置走"。
func (s *Store) SetRPMLimit(ctx context.Context, id int64, rpm int) (Key, error) {
	return s.update(ctx, `UPDATE api_keys SET rpm_limit = ? WHERE id = ? RETURNING *`, rpm, id)
}

// Disable 停用一个 Key，余额保留。
func (s *Store) Disable(ctx context.Context, id int64) (Key, error) {
	return s.update(ctx, `UPDATE api_keys SET disabled = TRUE WHERE id = ? RETURNING *`, id)
}

func (s *Store) find(ctx context.Context, where string, arg any) (Key, error) {
	var key Key
	err := s.db.WithContext(ctx).Where(where, arg).Take(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Key{}, ErrNotFound
	}
	return key, err
}

// update 执行一条带 RETURNING 的更新，一条也没改到就是这个 Key 不存在。
func (s *Store) update(ctx context.Context, sql string, args ...any) (Key, error) {
	var key Key
	res := s.db.WithContext(ctx).Raw(sql, args...).Scan(&key)
	switch {
	case res.Error != nil:
		return Key{}, res.Error
	case res.RowsAffected == 0:
		return Key{}, ErrNotFound
	}
	return key, nil
}
