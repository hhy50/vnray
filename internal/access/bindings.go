package access

import (
	"container/list"
	"context"
	"crypto/subtle"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
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

const userCacheCapacity = 4096

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
	key     string
	binding store.UserBinding
	found   bool
	expires time.Time
}

// Bindings 是管理面与数据面共用的访问服务：列表与写直达持久层，写成功后
// 同步通知观察者；用户凭据走短 TTL 缓存，客户端密钥走写穿内存索引。它实现
// transport.UserRegistry 与 core.SessionConfig。
type Bindings struct {
	globalClient atomic.Pointer[protocol.ClientConfig]
	store        store.Store
	authKeys     authKeyIndex

	// 观察者表只在装配期写入、此后只读，无需加锁。
	observers []UserObserver

	userCacheMu   sync.Mutex
	userWriteMu   sync.Mutex
	userVersion   uint64
	userCache     map[string]*list.Element
	userLRU       list.List
	clientWriteMu sync.Mutex
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
	return &Bindings{store: st, userCache: make(map[string]*list.Element)}
}

// queryCtx 返回带超时的操作上下文。
func queryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbTimeout)
}

// UserFor 实现 transport.UserRegistry：把自有协议用户出示的 user_key 解析成接入信息。
// 未注册或读库失败返回 ok=false（拒绝握手）；已停用的用户返回 ok=true 且 Active=false，
// 让调用方能区分"停用"与"没登记"。
func (b *Bindings) UserFor(ctx context.Context, key string) (transport.UserAccess, bool) {
	u, ok := b.LookupUser(ctx, store.ProtocolVNV1, key)
	if !ok {
		return transport.UserAccess{}, false
	}
	return transport.UserAccess{Client: u.Client, Name: u.Name, Active: u.Status == store.UserEnabled}, true
}

// cacheKey 把 (Protocol, Key) 拼成缓存索引键；NUL 分隔避免拼接歧义。
func cacheKey(p store.Protocol, key string) string { return string(p) + "\x00" + key }

// cachedUser 返回缓存里未过期的条目。
func (b *Bindings) cachedUser(p store.Protocol, key string) (userCacheEntry, bool, uint64) {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	k := cacheKey(p, key)
	e, ok := b.userCache[k]
	if !ok {
		return userCacheEntry{}, false, b.userVersion
	}
	value := e.Value.(userCacheEntry)
	if time.Now().After(value.expires) {
		delete(b.userCache, k)
		b.userLRU.Remove(e)
		return userCacheEntry{}, false, b.userVersion
	}
	b.userLRU.MoveToFront(e)
	return value, true, b.userVersion
}

// cacheUser 把一次点查结果（含"未注册"）写进缓存。
func (b *Bindings) cacheUser(p store.Protocol, key string, ub store.UserBinding, found bool, version uint64) bool {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	if version != b.userVersion {
		return false
	}
	k := cacheKey(p, key)
	value := userCacheEntry{key: k, binding: ub, found: found, expires: time.Now().Add(userCacheTTL)}
	if e, ok := b.userCache[k]; ok {
		e.Value = value
		b.userLRU.MoveToFront(e)
		return true
	}
	if len(b.userCache) >= userCacheCapacity {
		oldest := b.userLRU.Back()
		delete(b.userCache, oldest.Value.(userCacheEntry).key)
		b.userLRU.Remove(oldest)
	}
	b.userCache[k] = b.userLRU.PushFront(value)
	return true
}

// invalidateUser 在写路径上摘掉一条缓存，保证配置变更即时生效。
func (b *Bindings) invalidateUser(p store.Protocol, key string) {
	b.userCacheMu.Lock()
	defer b.userCacheMu.Unlock()
	b.userVersion++
	k := cacheKey(p, key)
	if e, ok := b.userCache[k]; ok {
		delete(b.userCache, k)
		b.userLRU.Remove(e)
	}
}

// LookupUser 按 store 的中性记录返回一条用户绑定（插件等外部消费者用）。
// 走 userCacheTTL 缓存：入站认证每连接一次点查，远程库下是唯一热路径；
// 配置变更由写路径 invalidateUser 兜底，绕过 Bindings 的直写最迟滞后一个 TTL。
func (b *Bindings) LookupUser(parent context.Context, p store.Protocol, key string) (store.UserBinding, bool) {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	for attempt := 0; ; attempt++ {
		e, ok, version := b.cachedUser(p, key)
		if ok {
			return e.binding, e.found
		}
		u, found, err := b.store.GetUser(ctx, p, key)
		if err != nil {
			logging.Warn("access: get user failed", "protocol", p, "err", err)
			return store.UserBinding{}, false
		}
		// 写入使在途旧查询失效；重读一次即可，持续写入时不该把这次查询拖成打库风暴。
		if b.cacheUser(p, key, u, found, version) || attempt > 0 {
			return u, found
		}
	}
}

// Lookup 返回一条用户记录的管理面视图。
func (b *Bindings) Lookup(ctx context.Context, p store.Protocol, key string) (Binding, bool) {
	u, ok := b.LookupUser(ctx, p, key)
	if !ok {
		return Binding{}, false
	}
	return b.viewOf(u), true
}

// PutUser 按 store 的中性记录写入一条用户绑定（为插件等外部消费者提供的窄入口）。
// 它是 SetUser 的搬运版：只做字段映射，语义完全一致（落库 + 通知观察者）。
func (b *Bindings) PutUser(ctx context.Context, u store.UserBinding) error {
	return b.SetUser(ctx, Binding{
		UserKey: u.Key, Protocol: u.Protocol, Name: u.Name, Status: u.Status,
		Client: u.Client, Config: u.Config,
	})
}

// ListUsers 返回全部用户的 store 中性记录（插件等外部消费者用）。
func (b *Bindings) ListUsers(parent context.Context) ([]store.UserBinding, error) {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.store.ListUsers(ctx)
}

// SetUser 新增或覆盖一条用户记录：先落库，成功后通知观察者。
func (b *Bindings) SetUser(parent context.Context, u Binding) error {
	b.userWriteMu.Lock()
	defer b.userWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.writeUserLocked(ctx, u)
}

// UpdateUser 在同一把写锁内读取、修改并写回一条用户记录：管理台按字段提交（改限速、
// 换规则、改密码）时不会互相覆盖。记录不存在时以 (protocol, key) 起手，即新增语义。
func (b *Bindings) UpdateUser(parent context.Context, p store.Protocol, key string, mutate func(*Binding) error) (Binding, error) {
	b.userWriteMu.Lock()
	defer b.userWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	cur := Binding{UserKey: key, Protocol: p}
	if row, ok, err := b.store.GetUser(ctx, p, key); err != nil {
		return Binding{}, err
	} else if ok {
		cur = b.viewOf(row)
	}
	if err := mutate(&cur); err != nil {
		return Binding{}, err
	}
	if err := b.writeUserLocked(ctx, cur); err != nil {
		return Binding{}, err
	}
	return cur, nil
}

// writeUserLocked 校验并写入一条绑定，成功后通知观察者；调用方必须持 userWriteMu。
func (b *Bindings) writeUserLocked(ctx context.Context, u Binding) error {
	if u.UserKey == "" || u.Client == "" {
		return fmt.Errorf("access: binding requires user_key and client")
	}
	if u.Protocol == "" {
		u.Protocol = store.ProtocolVNV1
	}
	if u.Name == "" {
		return fmt.Errorf("access: binding requires name")
	}
	if !u.Status.Valid() {
		return fmt.Errorf("access: binding status must be 0 or 1, got %d", int(u.Status))
	}
	item := store.UserBinding{Key: u.UserKey, Protocol: u.Protocol, Name: u.Name,
		Status: u.Status, Client: u.Client, Config: u.Config}
	err := b.store.SetUser(ctx, item)
	b.invalidateUser(u.Protocol, u.UserKey)
	if err != nil {
		return err
	}
	b.notify(UserChange{Protocol: u.Protocol, Key: u.UserKey, Name: u.Name,
		Status: u.Status, Client: u.Client, Config: u.Config})
	return nil
}

// DeleteUser 删除一条用户记录。
func (b *Bindings) DeleteUser(parent context.Context, p store.Protocol, key string) (bool, error) {
	b.userWriteMu.Lock()
	defer b.userWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	deleted, err := b.store.DeleteUser(ctx, p, key)
	b.invalidateUser(p, key)
	if err != nil {
		return false, err
	}
	if deleted {
		b.notify(UserChange{Protocol: p, Key: key, Deleted: true})
		// 已提交的删除须完成清理，即使请求随后断开。
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
		defer cleanupCancel()
		if _, err := b.store.DeleteUserTraffic(cleanupCtx, p, key); err != nil {
			logging.Warnf("access: delete user traffic: %v", err)
		}
	}
	return deleted, nil
}

// Snapshot 返回按 (protocol, user_key) 升序排列的全部用户；读库失败只告警并返回空表。
func (b *Bindings) Snapshot(parent context.Context) []Binding {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	rows, err := b.store.ListUsers(ctx)
	if err != nil {
		logging.Warnf("access: list users: %v", err)
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

// SetClient 新增或全量覆盖客户端注册记录；单列变更使用对应的 Save 方法。
func (b *Bindings) SetClient(parent context.Context, c ClientBinding) error {
	if c.ClientKey == "" {
		return fmt.Errorf("access: client binding requires client_key")
	}
	if c.ClientType == "" {
		c.ClientType = store.ClientTypeProxy
	}
	if c.ClientKey == transport.LocalClientName && c.ClientType != store.ClientTypeLocal {
		return fmt.Errorf("access: %q is reserved for the server proxy", transport.LocalClientName)
	}
	b.clientWriteMu.Lock()
	defer b.clientWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	item := store.ClientBinding{Key: c.ClientKey, Type: c.ClientType, Config: c.Config,
		ServerConfig: c.ServerConfig}
	return b.store.SetClient(ctx, item)
}

// LookupClient 返回一条客户端注册记录的管理面视图；不存在或读库失败时 ok 为 false。
func (b *Bindings) LookupClient(parent context.Context, clientKey string) (ClientBinding, bool) {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.lookupClient(ctx, clientKey)
}

func (b *Bindings) lookupClient(ctx context.Context, clientKey string) (ClientBinding, bool) {
	c, ok, err := b.store.GetClient(ctx, clientKey)
	if err != nil {
		logging.Warnf("access: get client %s: %v", clientKey, err)
		return ClientBinding{}, false
	}
	if !ok {
		return ClientBinding{}, false
	}
	return ClientBinding{ClientKey: c.Key, ClientType: c.Type, Config: c.Config,
		ServerConfig: c.ServerConfig, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}, true
}

// UserConfig 返回用户的运维配置（限速、日志级别等）；未注册或读库失败返回 false。
func (b *Bindings) UserConfig(ctx context.Context, p store.Protocol, key string) (store.UserConfig, bool) {
	u, ok := b.LookupUser(ctx, p, key)
	if !ok {
		return store.UserConfig{}, false
	}
	return u.Config, true
}

// ClientPushConfig 返回该客户端单独保存的配置，不包含全局继承值。
func (b *Bindings) ClientPushConfig(parent context.Context, clientKey string) protocol.ClientConfig {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.clientPushConfig(ctx, clientKey)
}

func (b *Bindings) clientPushConfig(ctx context.Context, clientKey string) protocol.ClientConfig {
	c, ok := b.lookupClient(ctx, clientKey)
	if !ok {
		return protocol.ClientConfig{}
	}
	cc := store.ParseClientConfig(c.ServerConfig)
	return protocol.ClientConfig{CustomHosts: cc.CustomHosts, IPMaps: cc.IPMaps}
}

// SaveClientReportedConfig 把客户端上报的配置快照写进 config 列：记录不存在时按
// proxy 类型补一条。ServerConfig 是服务端下发的运维值，不受上报影响。
func (b *Bindings) SaveClientReportedConfig(parent context.Context, clientKey, reported string) error {
	if clientKey == transport.LocalClientName {
		return fmt.Errorf("access: %q is reserved for the server proxy", clientKey)
	}
	ctx, cancel := queryCtx(parent)
	defer cancel()
	return b.store.SetClientReportedConfig(ctx, clientKey, reported)
}

// SaveClientServerConfig 更新运维配置，不覆盖并发上报的客户端快照。
func (b *Bindings) SaveClientServerConfig(parent context.Context, clientKey, settings string) error {
	b.clientWriteMu.Lock()
	defer b.clientWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	if clientKey == transport.LocalClientName {
		client, ok, err := b.store.GetClient(ctx, clientKey)
		if err != nil {
			return err
		}
		if !ok || client.Type != store.ClientTypeLocal {
			return fmt.Errorf("access: %q is reserved for the server proxy", clientKey)
		}
	}
	return b.store.SetClientServerConfig(ctx, clientKey, settings)
}

// UpdateClientServerConfig 在同一写锁内读取、修改并保存运维配置，保留客户端上报列。
func (b *Bindings) UpdateClientServerConfig(parent context.Context, clientKey string, mutate func(string) (string, error)) error {
	b.clientWriteMu.Lock()
	defer b.clientWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	client, found, err := b.store.GetClient(ctx, clientKey)
	if err != nil {
		return err
	}
	if clientKey == transport.LocalClientName && (!found || client.Type != store.ClientTypeLocal) {
		return fmt.Errorf("access: %q is reserved for the server proxy", clientKey)
	}
	next, err := mutate(client.ServerConfig)
	if err != nil {
		return err
	}
	return b.store.SetClientServerConfig(ctx, clientKey, next)
}

// DeleteClient 删除一条客户端注册记录。
func (b *Bindings) DeleteClient(parent context.Context, clientKey string) (bool, error) {
	b.clientWriteMu.Lock()
	defer b.clientWriteMu.Unlock()
	ctx, cancel := queryCtx(parent)
	defer cancel()
	deleted, err := b.store.DeleteClient(ctx, clientKey)
	if err != nil {
		return false, err
	}
	if deleted {
		if _, err := b.store.DeleteClientTraffic(ctx, clientKey); err != nil {
			logging.Warnf("access: delete client traffic: %v", err)
		}
	}
	return deleted, nil
}

// ClientSnapshot 返回按 client_key 升序排列的全部客户端；读库失败只告警并返回空表。
func (b *Bindings) ClientSnapshot(parent context.Context) []ClientBinding {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	rows, err := b.store.ListClients(ctx)
	if err != nil {
		logging.Warnf("access: list clients: %v", err)
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

// SetAuthKey 新增或按密钥覆盖一条接入密钥（同一密钥换范围即覆盖）。
// 密钥唯一，范围 key 不做唯一约束——允许多把密钥共用一个范围。
func (b *Bindings) SetAuthKey(parent context.Context, key, scope string) error {
	parsed, err := authkey.Parse(key)
	if err != nil {
		return err
	}
	if err := store.ValidateAuthScope(scope); err != nil {
		return err
	}
	ctx, cancel := queryCtx(parent)
	defer cancel()
	b.authKeys.mu.Lock()
	defer b.authKeys.mu.Unlock()
	if !b.authKeys.loaded {
		if err := b.loadAuthKeysLocked(ctx); err != nil {
			return err
		}
	}
	if err := b.store.SetAuthKey(ctx, store.AuthKey{Key: key, Scope: scope}); err != nil {
		b.authKeys.invalidate()
		return err
	}
	b.authKeys.rows[key] = struct{}{}
	b.authKeys.byID[parsed.ID()] = authKeyEntry{key: parsed, scope: scope}
	return nil
}

// DeleteAuthKey 删除一条接入密钥。
func (b *Bindings) DeleteAuthKey(parent context.Context, key string) (bool, error) {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	b.authKeys.mu.Lock()
	defer b.authKeys.mu.Unlock()
	if !b.authKeys.loaded {
		if err := b.loadAuthKeysLocked(ctx); err != nil {
			return false, err
		}
	}
	deleted, err := b.store.DeleteAuthKey(ctx, key)
	if err != nil {
		b.authKeys.invalidate()
		return false, err
	}
	delete(b.authKeys.rows, key)
	if parsed, err := authkey.Parse(key); err == nil {
		delete(b.authKeys.byID, parsed.ID())
	}
	return deleted, nil
}

// AuthKeySnapshot 返回按密钥升序排列的全部密钥；读库失败只告警并返回空表。
func (b *Bindings) AuthKeySnapshot(parent context.Context) []store.AuthKey {
	ctx, cancel := queryCtx(parent)
	defer cancel()
	keys, err := b.store.ListAuthKeys(ctx)
	if err != nil {
		logging.Warnf("access: list auth keys: %v", err)
		return nil
	}
	return keys
}

// MatchClientAuthKey 按索引校验密钥与范围；索引加载失败时拒绝配置文件回退。
func (b *Bindings) MatchClientAuthKey(ctx context.Context, clientName, key string) (configured, ok bool) {
	if err := b.lockAuthKeys(ctx); err != nil {
		return true, false
	}
	defer b.authKeys.mu.RUnlock()
	if len(b.authKeys.rows) == 0 {
		return false, false
	}
	parsed, err := authkey.Parse(key)
	if err != nil {
		return true, false
	}
	entry, found := b.authKeys.byID[parsed.ID()]
	return true, found && subtle.ConstantTimeCompare(entry.key[:], parsed[:]) == 1 && store.ScopeMatches(entry.scope, clientName)
}

// LookupTransportKey resolves a key without falling back when storage fails.
func (b *Bindings) LookupTransportKey(ctx context.Context, id authkey.ID) (authkey.Key, bool, error) {
	if err := b.lockAuthKeys(ctx); err != nil {
		return authkey.Key{}, true, err
	}
	defer b.authKeys.mu.RUnlock()
	if entry, found := b.authKeys.byID[id]; found {
		return entry.key, true, nil
	}
	if len(b.authKeys.rows) != 0 {
		return authkey.Key{}, true, errTransportKeyNotRegistered
	}
	return authkey.Key{}, false, nil
}
