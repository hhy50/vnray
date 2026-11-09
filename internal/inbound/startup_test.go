package inbound_test

import (
	"context"
	"testing"

	"github.com/hhy50/vnray/admin"
)

func TestManagerStartDoesNotSeed(t *testing.T) {
	st := testStore(t)
	m := newTestManager(st, admin.NewBindings(st), nil, nil)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Drain(context.Background())
	for _, status := range m.List() {
		if _, exists, err := st.GetPlugin(context.Background(), status.Name); err != nil || exists {
			t.Fatalf("Start created %s: %v", status.Name, err)
		}
	}
}
