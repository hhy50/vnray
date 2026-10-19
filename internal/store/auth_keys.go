package store

import "time"

// authKeyModel 是 auth_keys 表的 GORM 模型：auth_key（共享密钥）唯一，scope 是范围 key。
type authKeyModel struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Key       string    `gorm:"column:auth_key;size:255;not null;uniqueIndex"`
	Scope     string    `gorm:"column:scope;size:255;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (authKeyModel) TableName() string { return "vn_auth_keys" }

func (m authKeyModel) toAuthKey() AuthKey {
	return AuthKey{ID: m.ID, Key: m.Key, Scope: m.Scope, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
}
