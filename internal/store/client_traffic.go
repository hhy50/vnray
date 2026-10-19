package store

import "time"

// clientTrafficModel 是 client_traffic 表的 GORM 模型：客户端名即主键。
type clientTrafficModel struct {
	Key       string    `gorm:"column:client_key;size:255;primaryKey"`
	Up        int64     `gorm:"column:up_bytes;not null"`
	Down      int64     `gorm:"column:down_bytes;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (clientTrafficModel) TableName() string { return "vn_client_traffic" }
