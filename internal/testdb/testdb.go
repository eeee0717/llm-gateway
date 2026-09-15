// Package testdb 给测试提供数据库连接：连的是 docker compose 起的 MySQL，第一次使用时执行迁移。
package testdb

import (
	"database/sql"
	"os"
	"sync"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/eeee0717/llm-gateway/internal/config"
	"github.com/eeee0717/llm-gateway/migrations"
)

// dsnEnv 是测试数据库连接串的环境变量名，没设置时连本地 compose 起的那套。
const dsnEnv = "GATEWAY_TEST_DSN"

const defaultDSN = "gateway:gateway@tcp(localhost:3306)/gateway"

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
	// 连接串上的几个必须开关由 NormalizeDSN 补齐，和网关自己开库走的是同一套，见 internal/config。
	var dsn string
	if dsn, openErr = config.NormalizeDSN(DSN()); openErr != nil {
		return
	}
	sharedDB, openErr = gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard, TranslateError: true})
	if openErr != nil {
		return
	}
	var db *sql.DB
	if db, openErr = sharedDB.DB(); openErr != nil {
		return
	}
	// 这个池只服务测试自己的辅助查询——建 Key、查余额、查用量记录——都是一条一条来的。
	// 不设上限的话它会跟着被测的网关一起涨，两边抢同一批连接，
	// 先撞上 PostgreSQL 连接上限的可能是任何一方，排查起来就没了准头。
	db.SetMaxOpenConns(4)
	openErr = migrations.Up(db)
}
