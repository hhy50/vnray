package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

type canceledClientStore struct {
	store.Store
	entered chan struct{}
}

func (s *canceledClientStore) GetClient(ctx context.Context, _ string) (store.ClientBinding, bool, error) {
	close(s.entered)
	<-ctx.Done()
	return store.ClientBinding{}, false, ctx.Err()
}

type canceledConfigSession struct{ tcp.Session }

func (*canceledConfigSession) CloseChan() <-chan struct{} { return nil }

func TestClientConfigPushCancelsSnapshotRead(t *testing.T) {
	st := &canceledClientStore{entered: make(chan struct{})}
	bindings := NewBindings(st)
	client := transport.NewSession(&canceledConfigSession{}, "test", protocol.ClientInfo{Name: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bindings.PushClientConfigContext(ctx, client) }()
	select {
	case <-st.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot query did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("snapshot cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot query ignored push cancellation")
	}
}

type failedClientConfigStore struct {
	store.Store
	err error
}

func (s *failedClientConfigStore) GetClient(context.Context, string) (store.ClientBinding, bool, error) {
	return store.ClientBinding{}, false, s.err
}

func TestClientConfigReadFailurePreservesConcurrency(t *testing.T) {
	want := errors.New("database unavailable")
	bindings := NewBindings(&failedClientConfigStore{err: want})
	client := transport.NewSession(&canceledConfigSession{}, "test", protocol.ClientInfo{Name: "test"})
	client.SetClientConcurrency(7)
	if err := bindings.PushClientConfigContext(context.Background(), client); !errors.Is(err, want) {
		t.Fatalf("push error = %v, want %v", err, want)
	}
	if got := client.ClientConcurrency(); got != 7 {
		t.Fatalf("failed read changed concurrency to %d", got)
	}
}
