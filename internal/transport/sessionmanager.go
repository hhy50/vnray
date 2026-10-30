package transport

// SessionManager 维护所有认证通过的会话登记：tcp.Server 不再持表，登记与注销由
// 本管理器以 tcp.EventListener 的身份在会话事件里完成；按名选路与管理台快照都从这里读。

import (
	"errors"
	"fmt"
	"sync/atomic"
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

// snapshot 是登记表的不可变视图：写时整体重建并 CAS 换指针，读侧完全无锁。
// 会话开/关是低频事件，整表重建的代价可忽略。
type snapshot struct {
	sessions map[tcp.Session]*sessionEntry
	byName   map[string]*sessionEntry
	byID     map[uint64]*sessionEntry
	nextID   uint64
}

// SessionManager 维护所有认证通过的会话登记（客户端 + 用户），并实现 tcp.EventListener：
// Handshake 与流处理委托给注入的回调，登记/注销/索引由本管理器负责。
type SessionManager struct {
	// ClientReady 在会话登记后补发配置，覆盖认证应答与登记之间发生的更新。
	ClientReady func(*Session) error
	// Auth 与 Streams 由数据面服务在构造时注入：认证与流处理是业务决策，
	// 登记/注销/索引才是本管理器的职责。
	Auth    Authenticator
	Streams StreamHandler

	closed atomic.Bool
	snap   atomic.Pointer[snapshot]
}

// emptySnapshot 返回一张空表。
func emptySnapshot() *snapshot {
	return &snapshot{
		sessions: make(map[tcp.Session]*sessionEntry),
		byName:   make(map[string]*sessionEntry),
		byID:     make(map[uint64]*sessionEntry),
	}
}

// cloneSnapshot 复制一份快照供 COW 修改。
func cloneSnapshot(old *snapshot) *snapshot {
	next := &snapshot{
		sessions: make(map[tcp.Session]*sessionEntry, len(old.sessions)+1),
		byName:   make(map[string]*sessionEntry, len(old.byName)+1),
		byID:     make(map[uint64]*sessionEntry, len(old.byID)+1),
		nextID:   old.nextID,
	}
	for k, v := range old.sessions {
		next.sessions[k] = v
	}
	for k, v := range old.byName {
		next.byName[k] = v
	}
	for k, v := range old.byID {
		next.byID[k] = v
	}
	return next
}

// NewSessionManager 创建会话管理器；Auth 与 Streams 回调由数据面服务随后注入。
func NewSessionManager() *SessionManager {
	m := &SessionManager{}
	m.snap.Store(emptySnapshot())
	return m
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
// 并发握手用 CAS 重试串行化（冲突极罕见，重试即成功）。
func (m *SessionManager) OnSessionOpen(info *tcp.SessionInfo) error {
	if m.closed.Load() {
		return fmt.Errorf("transport: session manager is closed")
	}
	// 并发握手用 CAS 重试串行化（冲突极罕见，重试即成功）。
	var entry *sessionEntry
	for {
		old := m.snap.Load()
		if m.closed.Load() {
			return fmt.Errorf("transport: session manager is closed")
		}
		next := cloneSnapshot(old)
		info.Since = time.Now()
		entry = &sessionEntry{info: info, client: clientOf(info)}
		next.sessions[info.Session] = entry
		if info.Kind == tcp.KindClient {
			next.nextID++
			entry.id = next.nextID
			next.byID[entry.id] = entry
			if info.Name != "" {
				if prev, ok := next.byName[info.Name]; ok && prev != entry {
					logging.Warn("sessions: client name taken over", "name", info.Name, "remote", info.Remote)
				}
				next.byName[info.Name] = entry
			}
		}
		if m.snap.CompareAndSwap(old, next) {
			break
		}
	}

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
	if info.Kind == tcp.KindClient && entry.client != nil && m.ClientReady != nil {
		if err := m.ClientReady(entry.client); err != nil {
			info.Log.Warn("sessions: push client config failed", "err", err)
		}
	}
	return nil
}

// OnSessionClose 实现 tcp.EventListener：注销会话并连同按名/按编号索引。
func (m *SessionManager) OnSessionClose(info *tcp.SessionInfo) {
	for {
		old := m.snap.Load()
		entry, ok := old.sessions[info.Session]
		if !ok {
			// 管理器关闭时批量清过表，此处无事可注销。
			return
		}
		next := cloneSnapshot(old)
		delete(next.sessions, info.Session)
		if info.Kind == tcp.KindClient {
			if info.Name != "" && next.byName[info.Name] == entry {
				delete(next.byName, info.Name)
			}
			if entry.id != 0 && next.byID[entry.id] == entry {
				delete(next.byID, entry.id)
			}
		}
		if m.snap.CompareAndSwap(old, next) {
			break
		}
	}
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
	snap := m.snap.Load()
	out := make([]*tcp.SessionInfo, 0, len(snap.sessions))
	for _, entry := range snap.sessions {
		out = append(out, entry.info)
	}
	return out
}

// SessionByName 返回标识为 name 的在线客户端会话。
func (m *SessionManager) SessionByName(name string) (*tcp.SessionInfo, bool) {
	entry, ok := m.snap.Load().byName[name]
	if !ok {
		return nil, false
	}
	return entry.info, true
}

// Clients 返回在线客户端会话的编号化快照（按编号升序，供管理台组装视图）。
func (m *SessionManager) Clients() []ClientEntry {
	snap := m.snap.Load()
	out := make([]ClientEntry, 0, len(snap.byID))
	for id := uint64(1); id <= snap.nextID; id++ {
		entry, ok := snap.byID[id]
		if !ok {
			continue
		}
		out = append(out, ClientEntry{ID: id, Info: entry.info, Client: entry.client})
	}
	return out
}

// ClientCount 返回在线客户端会话数。
func (m *SessionManager) ClientCount() int {
	return len(m.snap.Load().byID)
}

// ClientSessions 返回在线客户端会话视图（心跳调度用）。
func (m *SessionManager) ClientSessions() []*Session {
	snap := m.snap.Load()
	out := make([]*Session, 0, len(snap.byID))
	for _, entry := range snap.byID {
		if entry.client != nil {
			out = append(out, entry.client)
		}
	}
	return out
}

// Close 关闭全部在册会话并拒绝后续登记（Drain 收尾用）；随后的事件注销为空操作。
func (m *SessionManager) Close() {
	m.closed.Store(true)
	for {
		old := m.snap.Load()
		infos := make([]*tcp.SessionInfo, 0, len(old.sessions))
		for _, entry := range old.sessions {
			infos = append(infos, entry.info)
		}
		if m.snap.CompareAndSwap(old, emptySnapshot()) {
			for _, info := range infos {
				_ = info.Session.Close()
			}
			return
		}
	}
}
