package portmap

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

type heldMappingSession struct {
	tcp.Session
	entered chan context.Context
	release <-chan struct{}
}

func (*heldMappingSession) CloseChan() <-chan struct{} { return nil }

func (s *heldMappingSession) OpenStreamContext(ctx context.Context) (tcp.Stream, error) {
	s.entered <- ctx
	<-ctx.Done()
	<-s.release
	return nil, ctx.Err()
}

func TestDrainWaitsForConnectionsAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		disableFirst bool
		cancelApp    bool
	}{
		{name: "drain"},
		{name: "disabled_then_drain", disableFirst: true},
		{name: "app_cancel", cancelApp: true},
		{name: "disabled_then_app_cancel", disableFirst: true, cancelApp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appctx, cancelApp := context.WithCancel(context.Background())
			defer cancelApp()
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			backend := &heldMappingSession{entered: make(chan context.Context, 1), release: release}
			sessions := transport.NewSessionManager()
			session := transport.NewSession(backend, "test", protocol.ClientInfo{Name: "edge"})
			if err := sessions.OnSessionOpen(&tcp.SessionInfo{Session: backend, Kind: tcp.KindClient, Name: "edge", Meta: session}); err != nil {
				t.Fatal(err)
			}
			st := &mappingSnapshotStore{rows: []store.PortMapping{
				{ID: 1, Enabled: true, Client: "edge", LocalHost: "127.0.0.1", LocalPort: 80},
			}}
			m := NewManager(appctx, sessions, st, nil, nil)
			t.Cleanup(func() {
				finish()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := m.Drain(ctx); err != nil {
					t.Error(err)
				}
			})
			m.Sync(context.Background())
			m.mu.Lock()
			entry := m.running[1]
			m.mu.Unlock()
			if entry == nil {
				t.Fatal("listener not started")
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(entry.ln.Addr().(*net.TCPAddr).Port))
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var opening context.Context
			select {
			case opening = <-backend.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("connection did not start opening a tunnel")
			}
			if tc.disableFirst {
				st.mu.Lock()
				st.rows[0].Enabled = false
				st.mu.Unlock()
				m.Sync(context.Background())
				if opening.Err() != nil {
					t.Fatal("disabling a listener canceled an accepted connection")
				}
				if len(m.ListeningIDs()) != 0 {
					t.Fatal("disabled listener remains registered")
				}
			}
			if tc.cancelApp {
				cancelApp()
				select {
				case <-opening.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("application cancellation did not reach tunnel opening")
				}
				mustWaitForMappingCount(t, m, 0)
			}
			deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = m.Drain(deadline)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Drain before connection cleanup = %v, want deadline exceeded", err)
			}
			select {
			case <-opening.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("Drain did not cancel tunnel opening")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			const waiters = 8
			results := make(chan error, waiters)
			for range waiters {
				go func() { results <- m.Drain(ctx) }()
			}
			finish()
			for range waiters {
				select {
				case err := <-results:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("concurrent Drain did not finish after connection cleanup")
				}
			}
			if err := m.Drain(ctx); err != nil {
				t.Fatalf("repeated Drain: %v", err)
			}
			if len(m.ListeningIDs()) != 0 {
				t.Fatal("Drain retained listeners")
			}
		})
	}
}

func mustWaitForMappingCount(t *testing.T, m *Manager, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(m.ListeningIDs()) != count {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("listeners = %v, want %d", m.ListeningIDs(), count)
		}
	}
}

func TestMappingCancellationIsIsolated(t *testing.T) {
	appctx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	st := &mappingSnapshotStore{rows: []store.PortMapping{
		{ID: 1, Enabled: true, Client: "offline", LocalHost: "127.0.0.1", LocalPort: 80},
		{ID: 2, Enabled: true, Client: "offline", LocalHost: "127.0.0.1", LocalPort: 81},
	}}
	m := NewManager(appctx, transport.NewSessionManager(), st, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := m.Start(appctx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	first, second := m.running[1], m.running[2]
	m.mu.Unlock()
	if first == nil || second == nil {
		t.Fatal("listeners not started")
	}
	first.cancel()
	mustWaitForMappingCount(t, m, 1)
	if m.ctx.Err() != nil || second.ctx.Err() != nil || !m.ListeningIDs()[2] {
		t.Fatal("canceling one mapping stopped its parent or sibling")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(second.ln.Addr().(*net.TCPAddr).Port))
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("sibling listener stopped: %v", err)
	}
	_ = conn.Close()
	cancelApp()
	mustWaitForMappingCount(t, m, 0)
	m.Sync(context.Background())
	if len(m.ListeningIDs()) != 0 {
		t.Fatal("application cancellation allowed listeners to restart")
	}
	if err := m.Start(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after application cancellation = %v", err)
	}
}

type mappingSnapshotStore struct {
	mu      sync.Mutex
	rows    []store.PortMapping
	entered chan struct{}
	release chan struct{}
}

func (s *mappingSnapshotStore) ListPortMappings(ctx context.Context) ([]store.PortMapping, error) {
	s.mu.Lock()
	rows := append([]store.PortMapping(nil), s.rows...)
	entered, release := s.entered, s.release
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return rows, nil
}

func TestDrainPreventsDelayedSyncFromReopening(t *testing.T) {
	st := &mappingSnapshotStore{
		rows:    []store.PortMapping{{ID: 1, Enabled: true, Client: "edge", LocalHost: "127.0.0.1", LocalPort: 80}},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	m := NewManager(context.Background(), transport.NewSessionManager(), st, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { m.Sync(ctx); close(done) }()
	select {
	case <-st.entered:
	case <-ctx.Done():
		t.Fatal("sync did not reach store")
	}
	if err := m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	close(st.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("delayed sync did not finish")
	}
	m.Sync(ctx)
	if got := m.ListeningIDs(); len(got) != 0 {
		t.Fatalf("drained manager reopened listeners: %v", got)
	}
}

func TestLimitReloadKeepsAcceptedConfigImmutable(t *testing.T) {
	st := &mappingSnapshotStore{rows: []store.PortMapping{
		{ID: 1, Enabled: true, Client: "offline", LocalHost: "127.0.0.1", LocalPort: 80},
	}}
	limits := ratelimit.NewRegistry()
	m := NewManager(context.Background(), transport.NewSessionManager(), st, nil, limits)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if err := m.Drain(ctx); err != nil {
			t.Error(err)
		}
	}()
	m.Sync(ctx)
	m.mu.Lock()
	e := m.running[1]
	m.mu.Unlock()
	if e == nil {
		t.Fatal("listener not started")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.ln.Addr().(*net.TCPAddr).Port))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 30 {
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}
	}()
	for i := int64(1); i <= 30; i++ {
		st.mu.Lock()
		st.rows[0].Config.UpLimit = i
		st.mu.Unlock()
		m.Sync(ctx)
	}
	wg.Wait()
	if e.cfg.Config.UpLimit != 0 {
		t.Fatal("listener snapshot mutated during reload")
	}
	if up, _ := limits.Limits(Identity(1)); up != 30 {
		t.Fatalf("limit = %d, want 30", up)
	}
}
