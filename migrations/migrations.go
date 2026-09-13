// Package migrations 存放 goose 的 SQL 迁移，并把它们 embed 进二进制，由 gateway migrate 执行。
package migrations

import (
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var files embed.FS

// Up 把数据库迁移到最新版本。goose 用 goose_db_version 表记录已经应用的迁移，重复执行会跳过它们。
func Up(db *sql.DB) error {
	goose.SetBaseFS(files)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}
