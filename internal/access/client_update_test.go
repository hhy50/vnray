package access

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/store"
)

func TestClientConfigUpdatesPreserveConcurrentFieldsAndReports(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "clients.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	b := NewBindings(st)
	if err := b.SetClient(ctx, ClientBinding{ClientKey: "edge", Config: "reported-v1", ServerConfig: "{\"count\":0,\"log_level\":\"debug\"}"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	const updates = 32
	for range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := b.UpdateClientServerConfig(ctx, "edge", func(raw string) (string, error) {
				var fields map[string]any
				if err := json.Unmarshal([]byte(raw), &fields); err != nil {
					return "", err
				}
				fields["count"] = fields["count"].(float64) + 1
				encoded, err := json.Marshal(fields)
				return string(encoded), err
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := b.SaveClientReportedConfig(ctx, "edge", "reported-v2"); err != nil {
			t.Error(err)
		}
	}()
	close(start)
	wg.Wait()
	client, ok, err := st.GetClient(ctx, "edge")
	if err != nil || !ok {
		t.Fatalf("client found=%v err=%v", ok, err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(client.ServerConfig), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["count"] != float64(updates) || fields["log_level"] != "debug" || client.Config != "reported-v2" {
		t.Fatalf("concurrent edits lost fields or report: %+v", client)
	}
}

func TestClientConfigUpdateCancelsReadBeforeMutation(t *testing.T) {
	st := &canceledClientStore{entered: make(chan struct{})}
	b := NewBindings(st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- b.UpdateClientServerConfig(ctx, "edge", func(string) (string, error) {
			t.Error("canceled read reached mutation")
			return "", nil
		})
	}()
	select {
	case <-st.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("configuration read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("update error = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration update ignored cancellation")
	}
}

func TestClientConfigMutationErrorDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "clients.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	b := NewBindings(st)
	if err := b.SaveClientServerConfig(ctx, "edge", "{\"log_level\":\"debug\"}"); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("invalid configuration")
	if err := b.UpdateClientServerConfig(ctx, "edge", func(string) (string, error) {
		return "", rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("mutation error = %v", err)
	}
	client, ok, err := st.GetClient(ctx, "edge")
	if err != nil || !ok || client.ServerConfig != "{\"log_level\":\"debug\"}" {
		t.Fatalf("rejected mutation changed client: %+v found=%v err=%v", client, ok, err)
	}
}
