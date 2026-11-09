package core

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/hhy50/vnray/internal/extension"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/traffic"
)

type lifecycleService struct {
	name   string
	events *[]string
	start  func() error
	drain  func(context.Context) error
}

func (s *lifecycleService) Name() string { return s.name }
func (s *lifecycleService) Start(context.Context) error {
	*s.events = append(*s.events, "start:"+s.name)
	if s.start != nil {
		return s.start()
	}
	return nil
}
func (s *lifecycleService) Drain(ctx context.Context) error {
	*s.events = append(*s.events, "drain:"+s.name)
	if s.drain != nil {
		return s.drain(ctx)
	}
	return nil
}

func TestInstanceStartupRollback(t *testing.T) {
	var events []string
	startErr, drainErr := errors.New("start failed"), errors.New("drain failed")
	services := []extension.Service{
		&lifecycleService{name: "first", events: &events, drain: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal("rollback used canceled startup context")
			}
			return nil
		}},
		&lifecycleService{name: "partial", events: &events, start: func() error { return startErr },
			drain: func(context.Context) error { return drainErr }},
		&lifecycleService{name: "unused", events: &events},
	}
	err := newInstance(context.Background(), services...).Run()
	if !errors.Is(err, startErr) || !errors.Is(err, drainErr) {
		t.Fatalf("lost startup or cleanup error: %v", err)
	}
	if want := []string{"start:first", "start:partial", "drain:partial", "drain:first"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestInstanceDrainsInDependencyOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	services := []extension.Service{
		&lifecycleService{name: "traffic", events: &events},
		&lifecycleService{name: "data", events: &events},
		&lifecycleService{name: "admin", events: &events, start: func() error { cancel(); return nil }},
	}
	if err := newInstance(ctx, services...).Run(); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:traffic", "start:data", "start:admin", "drain:admin", "drain:data", "drain:traffic"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestInstanceCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var events []string
	err := newInstance(ctx, &lifecycleService{name: "unused", events: &events}).Run()
	if !errors.Is(err, context.Canceled) || len(events) != 0 {
		t.Fatalf("err=%v events=%v", err, events)
	}
}

func TestDiagnosticsListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	svc := newPprofService(ln.Addr().String())
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("occupied diagnostics port reported success")
	}
	if err := svc.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type closeObservedStore struct {
	store.Store
	onClose func()
}

func (s *closeObservedStore) Close() error {
	s.onClose()
	return s.Store.Close()
}

func TestServerAppFlushesDrainedTrafficBeforeClosingStore(t *testing.T) {
	st := testStore(t)
	meter := traffic.NewMeter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	data := &lifecycleService{name: "data", events: &events, drain: func(context.Context) error {
		meter.RestoreUser(store.ProtocolVNV1, "alice")
		meter.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(123)
		return nil
	}}
	stopper := &lifecycleService{name: "admin", events: &events, start: func() error { cancel(); return nil }}
	closed := false
	observed := &closeObservedStore{Store: st, onClose: func() {
		rows, err := st.ListUserTraffic(context.Background())
		if err != nil || len(rows) != 1 || rows[0].Up != 123 {
			t.Errorf("store closed before final traffic flush: rows=%v err=%v", rows, err)
		}
		closed = true
	}}
	app := newServerApp(newInstance(ctx, traffic.NewService(meter, st), data, stopper), observed)
	if err := app.Run(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("store was not closed")
	}
}
