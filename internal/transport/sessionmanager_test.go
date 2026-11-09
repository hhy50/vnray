package transport

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/tcp"
)

// fakeSession 只满足登记表需要的会话契约，不承载任何数据面能力。
type fakeSession struct {
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func newFakeSession() *fakeSession { return &fakeSession{done: make(chan struct{})} }

func (s *fakeSession) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (s *fakeSession) OpenStream() (tcp.Stream, error)   { return nil, errors.New("no streams") }
func (s *fakeSession) AcceptStream() (tcp.Stream, error) { return nil, errors.New("no streams") }

func (s *fakeSession) CloseChan() <-chan struct{}  { return s.done }
func (s *fakeSession) IsClosed() bool              { return s.closed.Load() }
func (s *fakeSession) SetDeadline(time.Time) error { return nil }

func (s *fakeSession) Close() error {
	s.closed.Store(true)
	s.once.Do(func() { close(s.done) })
	return nil
}

func clientInfo(s tcp.Session, name string) *tcp.SessionInfo {
	return &tcp.SessionInfo{Session: s, Kind: tcp.KindClient, Name: name}
}

// TestSessionManagerKeepsIndexesInStep 断言登记表的三张索引随开/关同步增删。
func TestSessionManagerKeepsIndexesInStep(t *testing.T) {
	m := NewSessionManager()
	sess := newFakeSession()
	info := clientInfo(sess, "edge-1")
	if err := m.OnSessionOpen(info); err != nil {
		t.Fatalf("open: %v", err)
	}
	if m.ClientCount() != 1 {
		t.Fatalf("client count = %d, want 1", m.ClientCount())
	}
	if got, ok := m.SessionByName("edge-1"); !ok || got != info {
		t.Fatalf("by name = %v, %v", got, ok)
	}
	clients := m.Clients()
	if len(clients) != 1 || clients[0].ID != 1 || clients[0].Info != info {
		t.Fatalf("clients = %+v", clients)
	}
	if len(m.Snapshot()) != 1 {
		t.Fatalf("snapshot = %v", m.Snapshot())
	}

	m.OnSessionClose(info)
	if m.ClientCount() != 0 || len(m.Snapshot()) != 0 {
		t.Fatalf("closed session left behind: %d", m.ClientCount())
	}
	if _, ok := m.SessionByName("edge-1"); ok {
		t.Fatal("byName kept a closed session")
	}
}

// TestSessionManagerNameTakeover 断言同名重连时旧会话的注销不会摘掉新会话的索引。
func TestSessionManagerNameTakeover(t *testing.T) {
	m := NewSessionManager()
	older, newer := newFakeSession(), newFakeSession()
	oldInfo, newInfo := clientInfo(older, "edge-1"), clientInfo(newer, "edge-1")
	if err := m.OnSessionOpen(oldInfo); err != nil {
		t.Fatal(err)
	}
	if err := m.OnSessionOpen(newInfo); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.SessionByName("edge-1"); !ok || got != newInfo {
		t.Fatalf("by name = %v, want the newer session", got)
	}
	m.OnSessionClose(oldInfo)
	if got, ok := m.SessionByName("edge-1"); !ok || got != newInfo {
		t.Fatalf("closing the older session dropped the newer index: %v, %v", got, ok)
	}
	if m.ClientCount() != 1 {
		t.Fatalf("client count = %d, want 1", m.ClientCount())
	}
}

// TestSessionManagerConcurrentChurn 并发开关会话，断言索引不残留（配合 -race 使用）。
func TestSessionManagerConcurrentChurn(t *testing.T) {
	m := NewSessionManager()
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 32; n++ {
				sess := newFakeSession()
				info := clientInfo(sess, "edge")
				if err := m.OnSessionOpen(info); err != nil {
					t.Errorf("open: %v", err)
					return
				}
				_ = m.ClientCount()
				_ = m.Clients()
				m.OnSessionClose(info)
			}
		}()
	}
	wg.Wait()
	if got := m.ClientCount(); got != 0 {
		t.Fatalf("client count = %d after churn, want 0", got)
	}
	if _, ok := m.SessionByName("edge"); ok {
		t.Fatal("byName left over after churn")
	}
}

// TestSessionManagerRejectsAfterClose 断言收尾后不再接受登记。
func TestSessionManagerRejectsAfterClose(t *testing.T) {
	m := NewSessionManager()
	sess := newFakeSession()
	info := clientInfo(sess, "edge-1")
	if err := m.OnSessionOpen(info); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if !sess.closed.Load() {
		t.Fatal("Close did not close the registered session")
	}
	if err := m.OnSessionOpen(clientInfo(newFakeSession(), "edge-2")); err == nil {
		t.Fatal("closed manager accepted a new session")
	}
	// 关闭后的事件注销是空操作，不得 panic 或复活索引。
	m.OnSessionClose(info)
	if m.ClientCount() != 0 {
		t.Fatalf("client count = %d, want 0", m.ClientCount())
	}
}
