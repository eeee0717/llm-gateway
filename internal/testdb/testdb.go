// Package testdb 给测试提供数据库连接：连的是 docker compose 起的 PostgreSQL，第一次使用时执行迁移。
package testdb

import (
	"database/sql"
	"os"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/eeee0717/llm-gateway/migrations"
)

// dsnEnv 是测试数据库连接串的环境变量名，没设置时连本地 compose 起的那套。
const dsnEnv = "GATEWAY_TEST_DSN"

const defaultDSN = "postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable"

var (
	once     sync.Once
	sharedDB *gorm.DB
	openErr  error
)

// New 返回整个测试二进制共用的数据库连接，迁移只在第一次调用时执行。
// 测试之间不清库：每个测试自己创建数据，用随机值区分，所以可以并行。
func New(t testing.TB) *gorm.DB {
	t.Helper()
	once.Do(open)
	if openErr != nil {
		t.Fatalf("connect to test database (run: docker compose up -d): %v", openErr)
	}
	return sharedDB
}

// DSN 返回测试数据库的连接串，自己开连接池的测试用得上。
func DSN() string {
	if dsn := os.Getenv(dsnEnv); dsn != "" {
		return dsn
	}
	return defaultDSN
}

func open() {
	sharedDB, openErr = gorm.Open(postgres.Open(DSN()), &gorm.Config{Logger: logger.Discard})
	if openErr != nil {
		return
	}
	var db *sql.DB
	if db, openErr = sharedDB.DB(); openErr != nil {
		return
	}
	openErr = migrations.Up(db)
}
