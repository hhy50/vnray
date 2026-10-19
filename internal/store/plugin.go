package store

import (
	"database/sql/driver"
	"encoding/json"
	"time"
)

// PluginStatus 是插件的启停状态。
type PluginStatus string

const (
	// PluginDisabled 表示插件已停止监听。
	PluginDisabled PluginStatus = "disabled"
	// PluginEnabled 表示插件已开始监听。
	PluginEnabled PluginStatus = "enabled"
)

// PluginState 是 vn_plugin 表的一行：插件名唯一，Config 是插件私有参数。
type PluginState struct {
	Name      string       `gorm:"column:name;primaryKey;size:64"`
	Status    PluginStatus `gorm:"column:status;size:16;not null"`
	Config    PluginConfig `gorm:"column:config;type:text"`
	UpdatedAt time.Time    `gorm:"column:updated_at"`
}

func (PluginState) TableName() string { return "vn_plugin" }

// PluginConfig 是插件私有参数的 JSON 结构：Ports 按协议名索引各自的监听端口，
// Method 是 ss 的加密方法（其余插件不用）。
type PluginConfig struct {
	Ports  map[Protocol]int `json:"ports,omitempty"`
	Method string           `json:"method,omitempty"`
}

// ParsePluginConfig 解析插件配置 JSON；空串或非法 JSON 一律返回零值。
func ParsePluginConfig(raw string) PluginConfig {
	var c PluginConfig
	if raw == "" {
		return c
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return PluginConfig{}
	}
	return c
}

// Encode 序列化插件配置；全空返回空串，避免库里堆积空对象。
func (c PluginConfig) Encode() string {
	if len(c.Ports) == 0 && c.Method == "" {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// Value 实现 driver.Valuer：把配置写进 text 列。
func (c PluginConfig) Value() (driver.Value, error) { return c.Encode(), nil }

// Scan 实现 sql.Scanner：把 text 列读回配置；空串或非法 JSON 一律落成零值。
func (c *PluginConfig) Scan(value any) error {
	*c = PluginConfig{}
	b, err := configColumn(value)
	if err != nil || len(b) == 0 {
		return err
	}
	if err := json.Unmarshal(b, c); err != nil {
		*c = PluginConfig{}
	}
	return nil
}

// Normalize 把零值状态补齐成可写库的形态。
func (s PluginState) Normalize() PluginState {
	if s.Status == "" {
		s.Status = PluginDisabled
	}
	s.UpdatedAt = time.Now()
	return s
}
