package store

import "time"

// ClientBinding 是 vn_client_bindings 表的一行：client_key（客户端名）唯一。
// Config 是客户端上报的配置快照，ServerConfig 是服务端下发的运维配置。
type ClientBinding struct {
	ID           uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	Key          string     `gorm:"column:client_key;size:255;not null;uniqueIndex"`
	Type         ClientType `gorm:"column:client_type;size:32;not null"`
	Config       string     `gorm:"column:config;type:text"`
	ServerConfig string     `gorm:"column:server_config;type:text"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (ClientBinding) TableName() string { return "vn_client_bindings" }
