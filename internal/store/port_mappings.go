package store

import "time"

// PortMapping 是 vn_port_mappings 表的一行：remote_port 全表唯一，限速等运维配置收在
// Config JSON 列（见 PortConfig，键与 user_bindings.config 的 UserConfig 一致）。
type PortMapping struct {
	ID         uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	Client     string     `gorm:"column:client;size:255;not null;index"`
	RemotePort int        `gorm:"column:remote_port;not null;uniqueIndex"`
	LocalHost  string     `gorm:"column:local_host;size:255;not null"`
	LocalPort  int        `gorm:"column:local_port;not null"`
	Config     PortConfig `gorm:"column:config;type:text"`
	Enabled    bool       `gorm:"column:enabled;not null"`
	Remark     string     `gorm:"column:remark;size:255"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
}

func (PortMapping) TableName() string { return "vn_port_mappings" }
