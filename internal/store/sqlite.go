package store

import (
	"fmt"

	// 纯 Go 的 SQLite 方言（modernc 引擎），免 CGO 工具链。
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openSQLite 打开 SQLite 绑定表；dsn 为文件路径（含 ":memory:"），空串默认 "bindings.db"。
func openSQLite(dsn string) (*sqlStore, error) {
	if dsn == "" {
		dsn = "bindings.db"
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite %s: %w", dsn, err)
	}
	if sqlDB, err := db.DB(); err == nil {
		// SQLite 单写者：串行化访问避免 SQLITE_BUSY。
		sqlDB.SetMaxOpenConns(1)
	}
	return &sqlStore{db: db}, nil
}
