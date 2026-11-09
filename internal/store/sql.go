package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sql.go 只放 SQL 存储的装配与 CRUD；各表的 GORM 模型（表结构）在以表名命名的
// 同包文件里：user_bindings.go、client_bindings.go、auth_keys.go、plugin.go、
// user_traffic.go、client_traffic.go、port_mappings.go、service_settings.go。

// sqlStore 是基于 GORM 的实现，SQLite 与 MySQL 共用同一套语句，
// 方言差异（upsert / 建表）由 GORM 的 clause 与 AutoMigrate 吸收。
type sqlStore struct {
	db *gorm.DB
}

// GORMSource 是可选能力接口：SQL 存储暴露底层 GORM 连接，供管理台等
// 需要同库扩展表的组件复用（内存实现没有，断言失败即回退）。
type GORMSource interface {
	GORM() *gorm.DB
}

// GORM 返回底层连接；与 Store 共享连接池，调用方不得关闭它。
func (s *sqlStore) GORM() *gorm.DB { return s.db }

// Open 打开一个 SQL 存储；driver 支持 sqlite 与 mysql，dsn 语义由驱动决定。
// 返回值统一包一层 traceStore，查询日志只在那一处产生。
func Open(ctx context.Context, driver, dsn string, opts ...OpenOption) (Store, error) {
	var options openOptions
	for _, opt := range opts {
		opt(&options)
	}
	var st *sqlStore
	var err error
	switch driver {
	case "sqlite", "sqlite3":
		st, err = openSQLite(dsn)
	case "mysql":
		st, err = openMySQL(dsn)
	default:
		return nil, fmt.Errorf("store: unknown driver %q (want sqlite or mysql)", driver)
	}
	if err != nil {
		return nil, err
	}
	if err := st.initialize(ctx, options.initial); err != nil {
		_ = st.Close()
		return nil, err
	}
	return tracedStore{Store: st}, nil
}

func (s *sqlStore) SetUser(ctx context.Context, b UserBinding) error {
	if err := validateUser(b); err != nil {
		return err
	}
	now := time.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	// ID 交给自增主键；覆盖时 id/created_at 不在 DoUpdates 里，库中原值保留。
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "protocol"}, {Name: "user_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "status", "client", "config", "server_config", "updated_at"}),
	}).Create(&b).Error; err != nil {
		return fmt.Errorf("store: set user binding: %w", err)
	}
	return nil
}

func (s *sqlStore) GetUser(ctx context.Context, p Protocol, key string) (UserBinding, bool, error) {
	var m UserBinding
	err := s.db.WithContext(ctx).Where("protocol = ? AND user_key = ?", string(p), key).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UserBinding{}, false, nil
	}
	if err != nil {
		return UserBinding{}, false, fmt.Errorf("store: get user binding: %w", err)
	}
	return m, true, nil
}

func (s *sqlStore) DeleteUser(ctx context.Context, p Protocol, key string) (bool, error) {
	res := s.db.WithContext(ctx).Where("protocol = ? AND user_key = ?", string(p), key).Delete(&UserBinding{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete user binding: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) ListUsers(ctx context.Context) ([]UserBinding, error) {
	var models []UserBinding
	if err := s.db.WithContext(ctx).Order("protocol, user_key").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("store: list user bindings: %w", err)
	}
	return models, nil
}

func (s *sqlStore) SetClient(ctx context.Context, b ClientBinding) error {
	if err := validateClient(b); err != nil {
		return err
	}
	now := time.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"client_type", "config", "server_config", "updated_at"}),
	}).Create(&b).Error; err != nil {
		return fmt.Errorf("store: set client binding: %w", err)
	}
	return nil
}

func (s *sqlStore) GetClient(ctx context.Context, key string) (ClientBinding, bool, error) {
	var m ClientBinding
	err := s.db.WithContext(ctx).Where("client_key = ?", key).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ClientBinding{}, false, nil
	}
	if err != nil {
		return ClientBinding{}, false, fmt.Errorf("store: get client binding: %w", err)
	}
	return m, true, nil
}

// SetClientReportedConfig 更新客户端上报列。
func (s *sqlStore) SetClientReportedConfig(ctx context.Context, key, reported string) error {
	return s.setClientConfigColumn(ctx, key, reported, "config")
}

// SetClientServerConfig 更新运维配置列。
func (s *sqlStore) SetClientServerConfig(ctx context.Context, key, settings string) error {
	return s.setClientConfigColumn(ctx, key, settings, "server_config")
}

func (s *sqlStore) setClientConfigColumn(ctx context.Context, key, value, column string) error {
	b := ClientBinding{Key: key, Type: ClientTypeProxy}
	if err := validateClient(b); err != nil {
		return err
	}
	if column == "config" {
		b.Config = value
	} else {
		b.ServerConfig = value
	}
	b.CreatedAt = time.Now()
	b.UpdatedAt = b.CreatedAt
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client_key"}},
		DoUpdates: clause.AssignmentColumns([]string{column, "updated_at"}),
	}).Create(&b).Error; err != nil {
		return fmt.Errorf("store: set client %s: %w", column, err)
	}
	return nil
}

func (s *sqlStore) DeleteClient(ctx context.Context, key string) (bool, error) {
	res := s.db.WithContext(ctx).Where("client_key = ?", key).Delete(&ClientBinding{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete client binding: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) ListClients(ctx context.Context) ([]ClientBinding, error) {
	var models []ClientBinding
	if err := s.db.WithContext(ctx).Order("client_key").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("store: list client bindings: %w", err)
	}
	return models, nil
}

func (s *sqlStore) SetAuthKey(ctx context.Context, k AuthKey) error {
	if k.Key == "" {
		return fmt.Errorf("store: auth key requires key")
	}
	if err := ValidateAuthScope(k.Scope); err != nil {
		return err
	}
	now := time.Now()
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	k.UpdatedAt = now
	m := authKeyModel{Key: k.Key, Scope: k.Scope, CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "auth_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"scope", "updated_at"}),
	}).Create(&m).Error; err != nil {
		return fmt.Errorf("store: set auth key: %w", err)
	}
	return nil
}

func (s *sqlStore) GetAuthKey(ctx context.Context, key string) (AuthKey, bool, error) {
	var m authKeyModel
	err := s.db.WithContext(ctx).Where("auth_key = ?", key).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AuthKey{}, false, nil
	}
	if err != nil {
		return AuthKey{}, false, fmt.Errorf("store: get auth key: %w", err)
	}
	return m.toAuthKey(), true, nil
}

func (s *sqlStore) DeleteAuthKey(ctx context.Context, key string) (bool, error) {
	res := s.db.WithContext(ctx).Where("auth_key = ?", key).Delete(&authKeyModel{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete auth key: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) ListAuthKeys(ctx context.Context) ([]AuthKey, error) {
	var models []authKeyModel
	if err := s.db.WithContext(ctx).Order("auth_key").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("store: list auth keys: %w", err)
	}
	out := make([]AuthKey, 0, len(models))
	for _, m := range models {
		out = append(out, m.toAuthKey())
	}
	return out, nil
}

// addTraffic 把增量累加进一行流量记录：先 UPDATE，行不存在时 INSERT；并发插入
// 撞主键（另一实例抢先建行）时再累加一次。两条语句都不用方言特有的 upsert 语法。
func (s *sqlStore) addTraffic(ctx context.Context, model any, where string, args []any,
	up, down int64, insert func() error) error {
	for attempt := 0; attempt < 2; attempt++ {
		res := s.db.WithContext(ctx).Model(model).Where(where, args...).Updates(map[string]any{
			"up_bytes":   gorm.Expr("up_bytes + ?", up),
			"down_bytes": gorm.Expr("down_bytes + ?", down),
			"updated_at": time.Now(),
		})
		if res.Error != nil {
			return fmt.Errorf("store: add traffic: %w", res.Error)
		}
		if res.RowsAffected > 0 {
			return nil
		}
		err := insert()
		if err == nil {
			return nil
		}
		if attempt == 1 {
			return fmt.Errorf("store: add traffic: %w", err)
		}
	}
	return nil
}

// AddTrafficBatch 在单个事务里累加整批流量：逐条走独立事务时，SQLite 的每次提交
// 都是一次 fsync，一轮落库的代价随用户数线性增长。任一条失败整体回滚，调用方重试整批。
func (s *sqlStore) AddTrafficBatch(ctx context.Context, batch TrafficBatch) error {
	if batch.Empty() {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txStore := &sqlStore{db: tx}
		for _, d := range batch.Users {
			if err := txStore.AddUserTraffic(ctx, d.Protocol, d.Key, d.Up, d.Down); err != nil {
				return err
			}
		}
		for _, d := range batch.Clients {
			if err := txStore.AddClientTraffic(ctx, d.Key, d.Up, d.Down); err != nil {
				return err
			}
		}
		for _, d := range batch.PortMappings {
			if err := txStore.AddPortMappingTraffic(ctx, d.ID, d.Up, d.Down); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddUserTraffic 把一次增量累加进用户流量表。
func (s *sqlStore) AddUserTraffic(ctx context.Context, p Protocol, key string, up, down int64) error {
	if key == "" {
		return fmt.Errorf("store: user traffic requires key")
	}
	if up == 0 && down == 0 {
		return nil
	}
	return s.addTraffic(ctx, &userTrafficModel{}, "protocol = ? AND user_key = ?",
		[]any{string(p), key}, up, down, func() error {
			return s.db.WithContext(ctx).Create(&userTrafficModel{
				Protocol: string(p), Key: key, Up: up, Down: down, UpdatedAt: time.Now(),
			}).Error
		})
}

func (s *sqlStore) ListUserTraffic(ctx context.Context) ([]UserTraffic, error) {
	var rows []userTrafficModel
	if err := s.db.WithContext(ctx).Order("protocol, user_key").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list user traffic: %w", err)
	}
	out := make([]UserTraffic, 0, len(rows))
	for _, r := range rows {
		out = append(out, UserTraffic{Protocol: Protocol(r.Protocol), Key: r.Key, Up: r.Up, Down: r.Down, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

func (s *sqlStore) DeleteUserTraffic(ctx context.Context, p Protocol, key string) (bool, error) {
	res := s.db.WithContext(ctx).Where("protocol = ? AND user_key = ?", string(p), key).Delete(&userTrafficModel{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete user traffic: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) AddClientTraffic(ctx context.Context, key string, up, down int64) error {
	if key == "" {
		return fmt.Errorf("store: client traffic requires key")
	}
	if up == 0 && down == 0 {
		return nil
	}
	return s.addTraffic(ctx, &clientTrafficModel{}, "client_key = ?", []any{key}, up, down, func() error {
		return s.db.WithContext(ctx).Create(&clientTrafficModel{
			Key: key, Up: up, Down: down, UpdatedAt: time.Now(),
		}).Error
	})
}

func (s *sqlStore) ListClientTraffic(ctx context.Context) ([]ClientTraffic, error) {
	var rows []clientTrafficModel
	if err := s.db.WithContext(ctx).Order("client_key").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list client traffic: %w", err)
	}
	out := make([]ClientTraffic, 0, len(rows))
	for _, r := range rows {
		out = append(out, ClientTraffic{Key: r.Key, Up: r.Up, Down: r.Down, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

func (s *sqlStore) DeleteClientTraffic(ctx context.Context, key string) (bool, error) {
	res := s.db.WithContext(ctx).Where("client_key = ?", key).Delete(&clientTrafficModel{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete client traffic: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) SetPortMapping(ctx context.Context, m PortMapping) error {
	if err := validatePortMapping(m); err != nil {
		return err
	}
	// remote_port 全表唯一：先显式查冲突（比依赖数据库唯一索引报错更可读）。
	var dup PortMapping
	if err := s.db.WithContext(ctx).Where("remote_port = ? AND id <> ?", m.RemotePort, m.ID).First(&dup).Error; err == nil {
		return fmt.Errorf("store: remote port %d is already mapped to client %q (id %d)", m.RemotePort, dup.Client, dup.ID)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("store: check port mapping conflict: %w", err)
	}
	now := time.Now()
	if m.ID != 0 {
		res := s.db.WithContext(ctx).Model(&PortMapping{}).Where("id = ?", m.ID).Updates(map[string]any{
			"client": m.Client, "remote_port": m.RemotePort,
			"local_host": m.LocalHost, "local_port": m.LocalPort,
			"config":  m.Config,
			"enabled": m.Enabled, "remark": m.Remark, "updated_at": now,
		})
		if res.Error != nil {
			return fmt.Errorf("store: update port mapping: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("store: port mapping id %d not found", m.ID)
		}
		return nil
	}
	ins := PortMapping{Client: m.Client, RemotePort: m.RemotePort, LocalHost: m.LocalHost,
		LocalPort: m.LocalPort, Config: m.Config,
		Enabled: m.Enabled, Remark: m.Remark,
		CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).Create(&ins).Error; err != nil {
		return fmt.Errorf("store: insert port mapping: %w", err)
	}
	return nil
}

func (s *sqlStore) GetPortMapping(ctx context.Context, id uint64) (PortMapping, bool, error) {
	var m PortMapping
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PortMapping{}, false, nil
	}
	if err != nil {
		return PortMapping{}, false, fmt.Errorf("store: get port mapping: %w", err)
	}
	return m, true, nil
}

func (s *sqlStore) DeletePortMapping(ctx context.Context, id uint64) (bool, error) {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&PortMapping{})
	if res.Error != nil {
		return false, fmt.Errorf("store: delete port mapping: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *sqlStore) ListPortMappings(ctx context.Context) ([]PortMapping, error) {
	var models []PortMapping
	if err := s.db.WithContext(ctx).Order("remote_port").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("store: list port mappings: %w", err)
	}
	return models, nil
}

// AddPortMappingTraffic 原子累加现存映射的流量。
func (s *sqlStore) AddPortMappingTraffic(ctx context.Context, id uint64, up, down int64) error {
	if id == 0 || up < 0 || down < 0 {
		return fmt.Errorf("store: invalid port mapping traffic")
	}
	if up == 0 && down == 0 {
		return nil
	}
	// 只更新现存映射，避免删除后在途连接的迟到增量重新创建记录。
	err := s.db.WithContext(ctx).Model(&PortMapping{}).Where("id = ?", id).UpdateColumns(map[string]any{
		"up_bytes":   gorm.Expr("up_bytes + ?", up),
		"down_bytes": gorm.Expr("down_bytes + ?", down),
	}).Error
	if err != nil {
		return fmt.Errorf("store: add port mapping traffic: %w", err)
	}
	return nil
}

func (s *sqlStore) Close() error {
	if sqlDB, err := s.db.DB(); err == nil {
		return sqlDB.Close()
	}
	return nil
}

// GetPlugin 实现 Store：读取一个插件的持久状态。
func (s *sqlStore) GetPlugin(ctx context.Context, name string) (PluginState, bool, error) {
	var m PluginState
	err := s.db.WithContext(ctx).Where("name = ?", name).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PluginState{}, false, nil
	}
	if err != nil {
		return PluginState{}, false, fmt.Errorf("store: get plugin: %w", err)
	}
	return m, true, nil
}

// SetPlugin 实现 Store：按 name 覆盖一个插件的状态。
func (s *sqlStore) SetPlugin(ctx context.Context, st PluginState) error {
	if st.Name == "" {
		return fmt.Errorf("store: plugin state requires name")
	}
	st = st.Normalize()
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "config", "updated_at"}),
	}).Create(&st).Error; err != nil {
		return fmt.Errorf("store: set plugin: %w", err)
	}
	return nil
}

// ListPlugins 实现 Store：按 name 升序返回全部插件状态。
func (s *sqlStore) ListPlugins(ctx context.Context) ([]PluginState, error) {
	var rows []PluginState
	if err := s.db.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list plugins: %w", err)
	}
	return rows, nil
}

// GetServiceSettings 实现 Store：读取全部运行时配置段（段名 → 原文）。
func (s *sqlStore) GetServiceSettings(ctx context.Context) (map[string]string, error) {
	var rows []serviceSettingsModel
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: get service settings: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	return out, nil
}

// SetServiceSettings 实现 Store：按段 upsert，其它段保持不动；值为空串表示删除该段。
// 整批在一个事务里写，避免半套配置被读到。
func (s *sqlStore) SetServiceSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for key, value := range values {
			if value == "" {
				if err := tx.Where("setting_key = ?", key).Delete(&serviceSettingsModel{}).Error; err != nil {
					return err
				}
				continue
			}
			row := serviceSettingsModel{Key: key, Value: value, UpdatedAt: time.Now()}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "setting_key"}},
				DoUpdates: clause.AssignmentColumns([]string{"setting_value", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: set service settings: %w", err)
	}
	return nil
}
