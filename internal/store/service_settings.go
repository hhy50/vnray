package store

import "time"

// ServiceSettingsInitializedKey 是"默认值已落库"的标记键：表里存在这一行即表示初始化
// 做过，重启不再重刷，避免覆盖用户改过的值。
const ServiceSettingsInitializedKey = "initialized"

// serviceSettingsModel 是 vn_service_settings 表的一行：一段运行时配置。
// 整份配置按段拆成多行（public_host / heartbeat / vless …），写一段只动一行。
type serviceSettingsModel struct {
	Key       string    `gorm:"column:setting_key;primaryKey;size:64"`
	Value     string    `gorm:"column:setting_value;type:text"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (serviceSettingsModel) TableName() string { return "vn_service_settings" }
