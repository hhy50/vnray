package store

import "time"

// UserBinding 是 vn_user_bindings 表的一行：(protocol, user_key) 唯一，id 自增主键。
// Protocol 是接入协议，与 Key 一起构成身份——同一个 key 可以在不同协议下各有一行。
// Name 是给人看的显示名（非空、不唯一），Status 是启停状态。
// Config 是该用户的配置 JSON（见 UserConfig），ServerConfig 是服务端下发的覆盖值。
type UserBinding struct {
	ID           uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	Key          string     `gorm:"column:user_key;size:128;not null;uniqueIndex:idx_user_identity"`
	Protocol     Protocol   `gorm:"column:protocol;size:32;not null;uniqueIndex:idx_user_identity"`
	Name         string     `gorm:"column:name;size:128;not null"`
	Status       UserStatus `gorm:"column:status;not null"`
	Client       string     `gorm:"column:client;size:255;not null"`
	Config       UserConfig `gorm:"column:config;type:text"`
	ServerConfig string     `gorm:"column:server_config;type:text"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (UserBinding) TableName() string { return "vn_user_bindings" }
