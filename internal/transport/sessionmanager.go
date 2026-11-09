package transport

// SessionManager 维护所有认证通过的会话登记：tcp.Server 不再持表，登记与注销由
// 本管理器以 tcp.EventListener 的身份在会话事件里完成；按名选路与管理台快照都从这里读。

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/tcp"
)

// Authenticator 完成握手并返回会话元数据；由数据面服务注入（认证决策属于业务层）。
type Authenticator func(session tcp.Session) (tcp.SessionInfo, error)

// StreamHandler 处理一条入站流（可阻塞该流整个生命周期）；由数据面服务注入。
type StreamHandler func(info *tcp.SessionInfo, stream tcp.Stream) error

// ClientEntry 是登记表中一条客户端会话：编号 + 元数据 + 客户端视图（管理台视图）。
type ClientEntry struct {
	ID     uint64
	Info   *tcp.SessionInfo
	Client *Session
}

// sessionEntry 是登记表的一行：元数据、客户端视图与（客户端会话的）管理台编号。
type sessionEntry struct {
	info   *tcp.SessionInfo
	client *Session
	id     uint64
}

// SessionManager 维护所有认证通过的会话登记（客户端 + 用户），并实现 tcp.EventListener：
// Handshake 与流处理委托给注入的回调，登记/注销/索引由本管理器负责。
//
// 用户连接同样是会话，开/关随并发连接数持续发生，因此登记表用读写锁保护的普通 map，
// 写路径 O(1)；只有管理台这类低频读才按需复制切片、付出 O(N)。
type SessionManager struct {
	// ClientReady 在会话登记后补发配置，覆盖认证应答与登记之间发生的更新。
	ClientReady func(*Session) error
	// Auth 与 Streams 由数据面服务在构造时注入：认证与流处理是业务决策，
	// 登记/注销/索引才是本管理器的职责。
	Auth    Authenticator
	Streams StreamHandler

	mu       sync.RWMutex
	closed   bool
	nextID   uint64
	sessions map[tcp.Session]*sessionEntry
	byName   map[string]*sessionEntry
	byID     map[uint64]*sessionEntry
}

// NewSessionManager 创建会话管理器；Auth 与 Streams 回调由数据面服务随后注入。
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[tcp.Session]*sessionEntry),
		byName:   make(map[string]*sessionEntry),
		byID:     make(map[uint64]*sessionEntry),
	}
}

// clientOf 取出会话携带的客户端视图；只有客户端会话有。
func clientOf(info *tcp.SessionInfo) *Session {
	client, _ := info.Meta.(*Session)
	return client
}

// Handshake 实现 tcp.EventListener：委托给注入的认证器。
func (m *SessionManager) Handshake(session tcp.Session) (tcp.SessionInfo, error) {
	if m.Auth == nil {
		return tcp.SessionInfo{}, errors.New("transport: session manager has no authenticator")
	}
	return m.Auth(session)
}

// OnSessionOpen 实现 tcp.EventListener：登记会话（补全 Since），客户端会话分配编号
// 并建按名索引；管理器已关闭时返回 error，事件循环会据此拒绝该会话。
func (m *SessionManager) OnSessionOpen(info *tcp.SessionInfo) error {
	info.Since = time.Now()
	entry := &sessionEntry{info: info, client: clientOf(info)}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("transport: session manager is closed")
	}
	m.sessions[info.Session] = entry
	if info.Kind == tcp.KindClient {
		m.nextID++
		entry.id = m.nextID
		m.byID[entry.id] = entry
		if info.Name != "" {
			if prev, ok := m.byName[info.Name]; ok && prev != entry {
				logging.Warn("sessions: client name taken over", "name", info.Name, "remote", info.Remote)
			}
			m.byName[info.Name] = entry
		}
	}
	m.mu.Unlock()

	switch info.Kind {
	case tcp.KindClient:
		if entry.client != nil {
			reported := entry.client.Info()
			info.Log.Info("sessions: client session established", "remote", info.Remote,
				"version", reported.Version, "os", reported.OS, "arch", reported.Arch, "hostname", reported.Hostname)
		}
	default:
		attrs := []any{"remote", info.Remote, "bound client", info.Name}
		if meta, ok := info.Meta.(UserMeta); ok {
			attrs = append(attrs, "name", meta.Name, "version", meta.Info.Version, "os", meta.Info.OS,
				"arch", meta.Info.Arch, "hostname", meta.Info.Hostname)
		}
		info.Log.Info("sessions: user session established", attrs...)
	}
	// 补发配置要走网络，不能压在登记表锁里。
	if info.Kind == tcp.KindClient && entry.client != nil && m.ClientReady != nil {
		if err := m.ClientReady(entry.client); err != nil {
			info.Log.Warn("sessions: push client config failed", "err", err)
		}
	}
	return nil
}

// OnSessionClose 实现 tcp.EventListener：注销会话并连同按名/按编号索引。
func (m *SessionManager) OnSessionClose(info *tcp.SessionInfo) {
	m.mu.Lock()
	entry, ok := m.sessions[info.Session]
	if ok {
		delete(m.sessions, info.Session)
		if info.Kind == tcp.KindClient {
			if info.Name != "" && m.byName[info.Name] == entry {
				delete(m.byName, info.Name)
			}
			if entry.id != 0 && m.byID[entry.id] == entry {
				delete(m.byID, entry.id)
			}
		}
	}
	m.mu.Unlock()
	// 管理器关闭时已批量清表，此时只留一条关闭日志。
	switch info.Kind {
	case tcp.KindClient:
		info.Log.Info("sessions: client session closed", "remote", info.Remote)
	default:
		info.Log.Info("sessions: user session closed", "remote", info.Remote)
	}
}

// OnStreamOpen 实现 tcp.EventListener：委托给注入的流处理器。
func (m *SessionManager) OnStreamOpen(info *tcp.SessionInfo, stream tcp.Stream) error {
	if m.Streams == nil {
		return errors.New("transport: session manager has no stream handler")
	}
	return m.Streams(info, stream)
}

// OnStreamClose 实现 tcp.EventListener，只记一条日志。
func (m *SessionManager) OnStreamClose(info *tcp.SessionInfo, stream tcp.Stream) {
	info.Log.Debug("sessions: inbound stream closed", "remote", stream.RemoteAddr().String())
}

// Snapshot 返回当前全部认证会话的快照（客户端 + 用户）。
func (m *SessionManager) Snapshot() []*tcp.SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*tcp.SessionInfo, 0, len(m.sessions))
	for _, entry := range m.sessions {
		out = append(out, entry.info)
	}
	return out
}

// SessionByName 返回标识为 name 的在线客户端会话。
func (m *SessionManager) SessionByName(name string) (*tcp.SessionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.byName[name]
	if !ok {
		return nil, false
	}
	return entry.info, true
}

// Clients 返回在线客户端会话的编号化快照（按编号升序，供管理台组装视图）。
func (m *SessionManager) Clients() []ClientEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ClientEntry, 0, len(m.byID))
	for id, entry := range m.byID {
		out = append(out, ClientEntry{ID: id, Info: entry.info, Client: entry.client})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClientCount 返回在线客户端会话数。
func (m *SessionManager) ClientCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// ClientSessions 返回在线客户端会话视图（心跳调度用）。
func (m *SessionManager) ClientSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.byID))
	for _, entry := range m.byID {
		if entry.client != nil {
			out = append(out, entry.client)
		}
	}
	return out
}

// Close 关闭全部在册会话并拒绝后续登记（Drain 收尾用）；随后的事件注销为空操作。
// 关闭动作放在锁外：会话关闭会等到对端看到关闭，不该让登记表跟着停摆。
func (m *SessionManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	infos := make([]*tcp.SessionInfo, 0, len(m.sessions))
	for _, entry := range m.sessions {
		infos = append(infos, entry.info)
	}
	m.sessions = make(map[tcp.Session]*sessionEntry)
	m.byName = make(map[string]*sessionEntry)
	m.byID = make(map[uint64]*sessionEntry)
	m.mu.Unlock()

	for _, info := range infos {
		_ = info.Session.Close()
	}
}
