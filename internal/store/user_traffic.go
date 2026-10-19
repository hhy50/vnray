package store

import "time"

// userTrafficModel 是 user_traffic 表的 GORM 模型：(protocol, user_key) 复合主键，
// 两个字节数列是历次增量的累加结果。
type userTrafficModel struct {
	Protocol  string    `gorm:"column:protocol;size:32;primaryKey"`
	Key       string    `gorm:"column:user_key;size:128;primaryKey"`
	Up        int64     `gorm:"column:up_bytes;not null"`
	Down      int64     `gorm:"column:down_bytes;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (userTrafficModel) TableName() string { return "vn_user_traffic" }
