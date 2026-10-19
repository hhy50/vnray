package store

import "time"

// serviceSettingsRowID 是单行运行时配置的固定主键：整份配置一次读写，不需要按段分行。
const serviceSettingsRowID = 1

// serviceSettingsModel 是 vn_service_settings 表的 GORM 模型：单行保存管理台在线
// 修改的服务端运行时配置（整份 JSON 文本，避免同类 KV 的定长列装不下）。
type serviceSettingsModel struct {
	ID        uint   `gorm:"primaryKey"`
	Value     string `gorm:"column:setting_value;type:text"`
	UpdatedAt time.Time
}

func (serviceSettingsModel) TableName() string { return "vn_service_settings" }
