package admin

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"gorm.io/gorm"
)

// Binding 是一条用户记录的管理面视图：身份 = (Protocol, UserKey)。
type Binding struct {
	UserKey   string
	Protocol  store.Protocol
	Name      string
	Status    store.UserStatus
	Client    string
	Config    store.UserConfig
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ClientBinding 是一条客户端注册记录的管理面视图。
// Config 是客户端上报的配置快照，ServerConfig 是服务端下发的运维配置。
type ClientBinding struct {
	ClientKey    string
	ClientType   store.ClientType
	Config       string
	ServerConfig string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// dbTimeout 是单次读库/写库的最长等待；热路径上的查询不能卡住握手太久。
const dbTimeout = 5 * time.Second

// userCacheTTL 是凭据点查缓存的存活期。热路径（入站认证）每连接查一次用户表，
// 远程库下延迟可观；缓存期间配置变更经写路径失效兜底，最多滞后一个 TTL。
const userCacheTTL = 2 * time.Minute

// UserChange 描述一条用户记录的一次变更；Deleted 为 true 时其余字段无意义。
type UserChange struct {
	Protocol store.Protocol
	Key      string
	Name     string
	Status   store.UserStatus
	Client   string
	Config   store.UserConfig
	Deleted  bool
}

// UserObserver 在用户记录落库成功后收到通知，用于把配置推给运行时消费者（如限速注册表）。
// 回调在写路径上同步执行，实现必须快速返回、不得反向调用 Bindings。
type UserObserver func(UserChange)

// userCacheEntry 是凭据点查缓存的一行；found 为 false 缓存"未注册"的负结果，
// 避免陌生 key 反复打库。
type userCacheEntry struct {
	binding store.UserBinding
	found   bool
	expires time.Time
}

// Bindings 是绑定表的管理面门面：列表与写直达持久层（sqlite/mysql），写成功后
// 同步通知观察者；仅凭据点查（LookupUser）走短 TTL 缓存。它实现
// transport.UserRegistry 与 core.SessionConfig。
type Bindings struct {
	store store.Store

	// 观察者表只在装配期写入、此后只读，无需加锁。
	observers []UserObserver

	userCacheMu sync.Mutex
	userCache   map[string]userCacheEntry
}

// Watch 注册一个用户变更观察者；须在服务启动前调用。
func (b *Bindings) Watch(fn UserObserver) { b.observers = append(b.observers, fn) }

// notify 把一条用户变更推给全部观察者。
func (b *Bindings) notify(c UserChange) {
	for _, fn := range b.observers {
		fn(c)
	}
}

// NewBindings 打开绑定表门面；存储打不开由调用方在装配期处理，这里不做预加载。
func NewBindings(st store.Store) *Bindings {
	return &Bindings{store: st, userCache: map[string]userCacheEntry{}}
}

// queryCtx 返回带超时的操作上下文。
func queryCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

// UserFor 实现 transport.UserRegistry：把自有协议用户出示的 user_key 解析成接入信息。
// 未注册或读库失败返回 ok=false（拒绝握手）；已停用的用户返回 ok=true 且 Active=false，
// 让调用方能区分"停用"与"没登记"。
func (b *Bindings) UserFor(key string) (transport.UserAccess, bool) {
	u, ok := b.LookupUser(store.ProtocolVNV1, key)
	if !ok {
		return transport.UserAccess{}, false
	}
	return transport.UserAccess{Client: u.Client, Name: u.Name, Active: u.Status == store.UserEnabled}, true
}

// cacheKey 把 (Protocol, Key) 拼成缓存索引键；NUL 分隔避免拼接歧义。
func cacheKey(p store.Protocol, key string) string { return string(p) + "\x00" + key }

// cachedUser 返回缓存里未过期的条目。
func (b *Bindings) cachedUser(p store.Protocol, key string) (userCacheEntry, bool) {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	e, ok := b.userCache[cacheKey(p, key)]
	if !ok || time.Now().After(e.expires) {
		return userCacheEntry{}, false
	}
	return e, true
}

// cacheUser 把一次点查结果（含"未注册"）写进缓存。
func (b *Bindings) cacheUser(p store.Protocol, key string, ub store.UserBinding, found bool) {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	b.userCache[cacheKey(p, key)] = userCacheEntry{binding: ub, found: found,
		expires: time.Now().Add(userCacheTTL)}
}

// invalidateUser 在写路径上摘掉一条缓存，保证配置变更即时生效。
func (b *Bindings) invalidateUser(p store.Protocol, key string) {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	delete(b.userCache, cacheKey(p, key))
}

// LookupUser 按 store 的中性记录返回一条用户绑定（插件等外部消费者用）。
// 走 userCacheTTL 缓存：入站认证每连接一次点查，远程库下是唯一热路径；
// 配置变更由写路径 invalidateUser 兜底，绕过 Bindings 的直写最迟滞后一个 TTL。
func (b *Bindings) LookupUser(p store.Protocol, key string) (store.UserBinding, bool) {
	if e, ok := b.cachedUser(p, key); ok {
		return e.binding, e.found
	}
	ctx, cancel := queryCtx()
	defer cancel()
	u, ok, err := b.store.GetUser(ctx, p, key)
	if err != nil {
		logging.Warnf("admin: get user %s/%s: %v", p, key, err)
		return store.UserBinding{}, false
	}
	b.cacheUser(p, key, u, ok)
	return u, ok
}

// Lookup 返回一条用户记录的管理面视图。
func (b *Bindings) Lookup(p store.Protocol, key string) (Binding, bool) {
	u, ok := b.LookupUser(p, key)
	if !ok {
		return Binding{}, false
	}
	return b.viewOf(u), true
}

// PutUser 按 store 的中性记录写入一条用户绑定（为插件等外部消费者提供的窄入口）。
// 它是 SetUser 的搬运版：只做字段映射，语义完全一致（落库 + 通知观察者）。
func (b *Bindings) PutUser(u store.UserBinding) error {
	return b.SetUser(Binding{
		UserKey: u.Key, Protocol: u.Protocol, Name: u.Name, Status: u.Status,
		Client: u.Client, Config: u.Config,
	})
}

// ListUsers 返回全部用户的 store 中性记录（插件等外部消费者用）。
func (b *Bindings) ListUsers() []store.UserBinding {
	all := b.Snapshot()
	out := make([]store.UserBinding, 0, len(all))
	for _, v := range all {
		out = append(out, store.UserBinding{Key: v.UserKey, Protocol: v.Protocol,
			Name: v.Name, Status: v.Status, Client: v.Client, Config: v.Config})
	}
	return out
}

// SetUser 新增或覆盖一条用户记录：先落库，成功后通知观察者。
func (b *Bindings) SetUser(u Binding) error {
	if u.UserKey == "" || u.Client == "" {
		return fmt.Errorf("admin: binding requires user_key and client")
	}
	if u.Protocol == "" {
		u.Protocol = store.ProtocolVNV1
	}
	if u.Name == "" {
		return fmt.Errorf("admin: binding requires name")
	}
	if !u.Status.Valid() {
		return fmt.Errorf("admin: binding status must be 0 or 1, got %d", int(u.Status))
	}
	ctx, cancel := queryCtx()
	defer cancel()
	item := store.UserBinding{Key: u.UserKey, Protocol: u.Protocol, Name: u.Name,
		Status: u.Status, Client: u.Client, Config: u.Config}
	if err := b.store.SetUser(ctx, item); err != nil {
		return err
	}
	b.invalidateUser(u.Protocol, u.UserKey)
	b.notify(UserChange{Protocol: u.Protocol, Key: u.UserKey, Name: u.Name,
		Status: u.Status, Client: u.Client, Config: u.Config})
	return nil
}

// DeleteUser 删除一条用户记录。
func (b *Bindings) DeleteUser(p store.Protocol, key string) (bool, error) {
	ctx, cancel := queryCtx()
	defer cancel()
	deleted, err := b.store.DeleteUser(ctx, p, key)
	if err != nil {
		return false, err
	}
	if deleted {
		// 用户没了，他的流量记录也一并删掉；失败只告警，不回滚绑定删除。
		if _, err := b.store.DeleteUserTraffic(ctx, p, key); err != nil {
			logging.Warnf("admin: delete user traffic: %v", err)
		}
		b.invalidateUser(p, key)
		b.notify(UserChange{Protocol: p, Key: key, Deleted: true})
	}
	return deleted, nil
}

// Snapshot 返回按 (protocol, user_key) 升序排列的全部用户；读库失败只告警并返回空表。
func (b *Bindings) Snapshot() []Binding {
	ctx, cancel := queryCtx()
	defer cancel()
	rows, err := b.store.ListUsers(ctx)
	if err != nil {
		logging.Warnf("admin: list users: %v", err)
		return nil
	}
	out := make([]Binding, 0, len(rows))
	for _, u := range rows {
		out = append(out, b.viewOf(u))
	}
	return out
}

// viewOf 把 store 中性记录翻成管理面视图。
func (b *Bindings) viewOf(u store.UserBinding) Binding {
	return Binding{UserKey: u.Key, Protocol: u.Protocol, Name: u.Name, Status: u.Status,
		Client: u.Client, Config: u.Config, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

// SetClient 新增或覆盖一条客户端注册记录。store 是全量 upsert，
// config/server_config 两列的"写其一不覆盖另一个"由这里的读改写保证。
func (b *Bindings) SetClient(c ClientBinding) error {
	if c.ClientKey == "" {
		return fmt.Errorf("admin: client binding requires client_key")
	}
	if c.ClientType == "" {
		c.ClientType = store.ClientTypeProxy
	}
	if c.ClientKey == transport.LocalClientName && c.ClientType != store.ClientTypeLocal {
		return fmt.Errorf("admin: %q is reserved for the server proxy", transport.LocalClientName)
	}
	ctx, cancel := queryCtx()
	defer cancel()
	item := store.ClientBinding{Key: c.ClientKey, Type: c.ClientType, Config: c.Config,
		ServerConfig: c.ServerConfig}
	return b.store.SetClient(ctx, item)
}

// LookupClient 返回一条客户端注册记录的管理面视图；不存在或读库失败时 ok 为 false。
func (b *Bindings) LookupClient(clientKey string) (ClientBinding, bool) {
	ctx, cancel := queryCtx()
	defer cancel()
	c, ok, err := b.store.GetClient(ctx, clientKey)
	if err != nil {
		logging.Warnf("admin: get client %s: %v", clientKey, err)
		return ClientBinding{}, false
	}
	if !ok {
		return ClientBinding{}, false
	}
	return ClientBinding{ClientKey: c.Key, ClientType: c.Type, Config: c.Config,
		ServerConfig: c.ServerConfig, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}, true
}

// UserConfig 返回用户的运维配置（限速、日志级别等）；未注册或读库失败返回 false。
func (b *Bindings) UserConfig(p store.Protocol, key string) (store.UserConfig, bool) {
	u, ok := b.LookupUser(p, key)
	if !ok {
		return store.UserConfig{}, false
	}
	return u.Config, true
}

// ClientLogLevel 返回服务端下发的日志级别；未注册、没配或读库失败时返回空串（视为 info）。
func (b *Bindings) ClientLogLevel(clientKey string) string {
	c, ok := b.LookupClient(clientKey)
	if !ok {
		return ""
	}
	return store.ParseClientConfig(c.ServerConfig).LogLevel
}

// ClientConcurrency 返回服务端下发的该客户端并发上限；未注册、没配或读库失败时返回 0（不限）。
func (b *Bindings) ClientConcurrency(clientKey string) int {
	c, ok := b.LookupClient(clientKey)
	if !ok {
		return 0
	}
	return store.ParseClientConfig(c.ServerConfig).Concurrency
}

// PushConfig 返回服务端下发给该客户端的全量配置快照；未注册、没配或读库失败时为空。
// 只暴露客户端需要消费的字段（自定义 host 表与 IP 段映射），后续配置项在这里追加。
func (b *Bindings) PushConfig(clientKey string) protocol.PushConfig {
	c, ok := b.LookupClient(clientKey)
	if !ok {
		return protocol.PushConfig{}
	}
	cc := store.ParseClientConfig(c.ServerConfig)
	return protocol.PushConfig{CustomHosts: cc.CustomHosts, IPMaps: cc.IPMaps}
}

// SaveClientReportedConfig 把客户端上报的配置快照写进 config 列：记录不存在时按
// proxy 类型补一条。ServerConfig 是服务端下发的运维值，不受上报影响。
func (b *Bindings) SaveClientReportedConfig(clientKey, reported string) error {
	cur, ok := b.LookupClient(clientKey)
	if !ok {
		cur = ClientBinding{ClientKey: clientKey, ClientType: store.ClientTypeProxy}
	}
	cur.Config = reported
	return b.SetClient(cur)
}

// DeleteClient 删除一条客户端注册记录。
func (b *Bindings) DeleteClient(clientKey string) (bool, error) {
	ctx, cancel := queryCtx()
	defer cancel()
	deleted, err := b.store.DeleteClient(ctx, clientKey)
	if err != nil {
		return false, err
	}
	if deleted {
		if _, err := b.store.DeleteClientTraffic(ctx, clientKey); err != nil {
			logging.Warnf("admin: delete client traffic: %v", err)
		}
	}
	return deleted, nil
}

// ClientSnapshot 返回按 client_key 升序排列的全部客户端；读库失败只告警并返回空表。
func (b *Bindings) ClientSnapshot() []ClientBinding {
	ctx, cancel := queryCtx()
	defer cancel()
	rows, err := b.store.ListClients(ctx)
	if err != nil {
		logging.Warnf("admin: list clients: %v", err)
		return nil
	}
	out := make([]ClientBinding, 0, len(rows))
	for _, c := range rows {
		out = append(out, ClientBinding{ClientKey: c.Key, ClientType: c.Type, Config: c.Config,
			ServerConfig: c.ServerConfig, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out
}

// Close 释放底层存储。
func (b *Bindings) Close() error { return b.store.Close() }

// --- 接入密钥（范围 key）---

// MinAuthKeyLen 是接入密钥的最小长度，与 config.Auth 的校验口径一致。
const MinAuthKeyLen = 8

// SetAuthKey 新增或按密钥覆盖一条接入密钥（同一密钥换范围即覆盖）。
// 密钥唯一，范围 key 不做唯一约束——允许多把密钥共用一个范围。
func (b *Bindings) SetAuthKey(key, scope string) error {
	if len(key) < MinAuthKeyLen {
		return fmt.Errorf("admin: auth key must be at least %d characters", MinAuthKeyLen)
	}
	if err := store.ValidateAuthScope(scope); err != nil {
		return err
	}
	ctx, cancel := queryCtx()
	defer cancel()
	return b.store.SetAuthKey(ctx, store.AuthKey{Key: key, Scope: scope})
}

// DeleteAuthKey 删除一条接入密钥。
func (b *Bindings) DeleteAuthKey(key string) (bool, error) {
	ctx, cancel := queryCtx()
	defer cancel()
	return b.store.DeleteAuthKey(ctx, key)
}

// AuthKeySnapshot 返回按密钥升序排列的全部密钥；读库失败只告警并返回空表。
func (b *Bindings) AuthKeySnapshot() []store.AuthKey {
	ctx, cancel := queryCtx()
	defer cancel()
	keys, err := b.store.ListAuthKeys(ctx)
	if err != nil {
		logging.Warnf("admin: list auth keys: %v", err)
		return nil
	}
	return keys
}

// MatchClientAuthKey 报告一次客户端认证的判定结果：configured 为 false 表示密钥表
// 为空（服务端未配置任何密钥，调用方回退旧校验路径）；否则 ok 即放行判定。
// 未命中范围的条目也参与比对，避免提前退出暴露"命中的是哪条范围"。
// 读库失败按已配置但拒绝处理：认证决策宁严勿宽，不因故障放行。
func (b *Bindings) MatchClientAuthKey(clientName, key string) (configured, ok bool) {
	keys := b.AuthKeySnapshot()
	if len(keys) == 0 {
		return false, false
	}
	matched := false
	for _, k := range keys {
		if !store.ScopeMatches(k.Scope, clientName) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Key), []byte(key)) == 1 {
			matched = true
		}
	}
	return true, matched
}

// GORM 返回底层存储的 GORM 连接；管理台用它把账号、审计、指标建进业务同一个库。
// 调用方拿不到连接（理论上只有存储实现异常）时返回 nil，装配层视为失败。
func (b *Bindings) GORM() *gorm.DB {
	if gs, ok := b.store.(store.GORMSource); ok {
		return gs.GORM()
	}
	return nil
}
