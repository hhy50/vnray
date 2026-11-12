package store

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	initializationVersion = 1
	authKeyInitializedKey = "auth_key_initialized"
	ruleSeededKey         = "rules.seeded"
)

// InitialData 是服务端装配提供的初始数据；只在初始化事务中使用。
type InitialData struct {
	AuthKey  AuthKey
	Settings map[string]string
	Rules    map[string]string
	Plugins  []PluginState
}

type openOptions struct{ initial *InitialData }

// OpenOption 配置数据库打开与初始化。
type OpenOption func(*openOptions)

// WithInitialData 在表结构准备完成后事务性地初始化服务端数据。
func WithInitialData(data InitialData) OpenOption {
	return func(o *openOptions) { o.initial = &data }
}

type initializationModel struct {
	ID      uint `gorm:"primaryKey;autoIncrement:false"`
	Version int  `gorm:"not null"`
}

func (initializationModel) TableName() string { return "vn_initialization" }

func (s *sqlStore) initialize(ctx context.Context, data *InitialData) error {
	// MySQL DDL 会隐式提交；先完成可重入建表，再开始数据事务。
	if err := s.db.WithContext(ctx).AutoMigrate(&UserBinding{}, &ClientBinding{}, &authKeyModel{},
		&PluginState{}, &userTrafficModel{}, &clientTrafficModel{}, &PortMapping{},
		&serviceSettingsModel{}, &initializationModel{}, &MonitoringSample{}); err != nil {
		return fmt.Errorf("store: migrate tables: %w", err)
	}
	if data == nil {
		return nil
	}
	return s.initializeData(ctx, *data)
}

func (s *sqlStore) initializeData(ctx context.Context, data InitialData) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		state := initializationModel{ID: 1}
		// 插入和锁行都在数据事务内，失败不会留下完成标记。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, 1).Error; err != nil {
			return err
		}
		if state.Version > initializationVersion {
			return fmt.Errorf("unsupported initialization version %d", state.Version)
		}
		if state.Version == initializationVersion {
			return nil
		}
		st := &sqlStore{db: tx}
		if err := st.initializeV1(ctx, data); err != nil {
			return err
		}
		return tx.Model(&state).Update("version", initializationVersion).Error
	})
	if err != nil {
		return fmt.Errorf("store: initialize database: %w", err)
	}
	return nil
}

func (s *sqlStore) initializeV1(ctx context.Context, data InitialData) error {
	kv, err := s.GetServiceSettings(ctx)
	if err != nil {
		return err
	}
	keys, err := s.ListAuthKeys(ctx)
	if err != nil {
		return err
	}
	hasSettings := false
	for key := range kv {
		if key != authKeyInitializedKey {
			hasSettings = true
			break
		}
	}
	if kv[authKeyInitializedKey] != "true" && !hasSettings && len(keys) == 0 {
		if err := s.SetAuthKey(ctx, data.AuthKey); err != nil {
			return err
		}
	}
	updates := map[string]string{}
	for key, raw := range kv {
		name, legacy := strings.CutPrefix(key, "user.rule.")
		if !legacy || name == "" {
			continue
		}
		target := "rule." + name
		if _, exists := kv[target]; !exists && raw != "" {
			updates[target], kv[target] = raw, raw
		}
		updates[key] = ""
	}
	if _, initialized := kv[ServiceSettingsInitializedKey]; !initialized {
		for key, value := range data.Settings {
			if _, exists := kv[key]; !exists {
				updates[key] = value
			}
		}
	}
	if _, seeded := kv[ruleSeededKey]; !seeded {
		for key, value := range data.Rules {
			if _, exists := kv[key]; !exists {
				updates[key] = value
			}
		}
	}
	for _, key := range []string{authKeyInitializedKey, ServiceSettingsInitializedKey, ruleSeededKey} {
		updates[key] = ""
	}
	if err := s.SetServiceSettings(ctx, updates); err != nil {
		return err
	}
	for _, state := range data.Plugins {
		if _, exists, err := s.GetPlugin(ctx, state.Name); err != nil {
			return err
		} else if exists {
			continue
		}
		if err := s.SetPlugin(ctx, state); err != nil {
			return err
		}
	}
	return nil
}
