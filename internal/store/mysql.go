package store

import (
	"context"
	"fmt"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openMySQL 打开 MySQL 绑定表；dsn 必填，形如 user:pass@tcp(host:port)/dbname。
func openMySQL(ctx context.Context, dsn string) (Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("store: mysql requires forward.storage.dsn")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("store: open mysql: %w", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(4)
		sqlDB.SetMaxIdleConns(2)
	}
	return newSQLStore(ctx, db)
}
