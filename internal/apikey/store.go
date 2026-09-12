package apikey

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Key 是一个 API Key 的记录。余额的单位是微元（1 微元 = 0.000001 元），见 docs/adr/0001。
type Key struct {
	ID           int64
	Name         string
	KeyHash      string
	BalanceMicro int64
	Disabled     bool
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

// Create 新建一个 Key。余额为零，管理员充值后才能用。
func (s *Store) Create(ctx context.Context, name, hash string) (Key, error) {
	key := Key{Name: name, KeyHash: hash}
	err := s.db.WithContext(ctx).Create(&key).Error
	return key, err
}
