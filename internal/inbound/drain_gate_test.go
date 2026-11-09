package inbound_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/inbound"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/sqliteweb"
	"github.com/hhy50/vnray/internal/store"
)

type blockingDrainPlugin struct {
	plugin.Plugin
	started chan struct{}
	release chan struct{}
}

func (*blockingDrainPlugin) Name() string { return "a-blocking" }
func (*blockingDrainPlugin) Stop()        {}
func (p *blockingDrainPlugin) Drain(ctx context.Context) error {
	close(p.started)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestManagerDrainRejectsPreviouslyFetchedPlugin(t *testing.T) {
	for _, name := range []string{"ss", "v2ray", "vnray-user", "sqlite-web"} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			var target plugin.Plugin
			params := map[string]string{"port": strconv.Itoa(freePort(t))}
			switch name {
			case "ss":
				target = inbound.NewSSPlugin(st, nil, nil, nil)
			case "v2ray":
				target = inbound.NewV2RayPlugin(st, nil, nil, nil)
				params = map[string]string{"vmess_port": strconv.Itoa(freePort(t)), "vless_port": strconv.Itoa(freePort(t))}
			case "vnray-user":
				target = inbound.NewVNRayUserPlugin(st, inbound.VNRayUserDependencies{})
			case "sqlite-web":
				target = sqliteweb.New(st)
			}
			blocker := &blockingDrainPlugin{started: make(chan struct{}), release: make(chan struct{})}
			manager := plugin.NewRegistry(context.Background(), blocker, target)
			previous, err := manager.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			drained := make(chan error, 1)
			go func() { drained <- manager.Drain(ctx) }()
			<-blocker.started
			defer func() {
				close(blocker.release)
				if err := <-drained; err != nil {
					t.Errorf("drain: %v", err)
				}
			}()
			if err := previous.Enable(ctx, params); !errors.Is(err, plugin.ErrDraining) {
				t.Fatalf("enable during manager drain: %v", err)
			}
			if err := previous.Disable(ctx); !errors.Is(err, plugin.ErrDraining) {
				t.Fatalf("disable during manager drain: %v", err)
			}
			if err := previous.Restore(ctx); !errors.Is(err, plugin.ErrDraining) {
				t.Fatalf("restore during manager drain: %v", err)
			}
			if _, err := manager.Get(name); !errors.Is(err, plugin.ErrDraining) {
				t.Fatalf("get during manager drain: %v", err)
			}
			if err := manager.Start(ctx); !errors.Is(err, plugin.ErrDraining) {
				t.Fatalf("start during manager drain: %v", err)
			}
			if _, exists, err := st.GetPlugin(ctx, name); exists || err != nil {
				t.Fatalf("draining plugin changed persisted state: exists=%v, err=%v", exists, err)
			}
			if setter, ok := previous.(plugin.PortSetter); ok {
				if err := setter.SetPort(ctx, freePort(t)); !errors.Is(err, plugin.ErrDraining) {
					t.Fatalf("set port during manager drain: %v", err)
				}
			}
			if setter, ok := previous.(plugin.MultiPortSetter); ok {
				if err := setter.SetPorts(ctx, map[store.Protocol]int{store.ProtocolVMess: freePort(t)}); !errors.Is(err, plugin.ErrDraining) {
					t.Fatalf("set ports during manager drain: %v", err)
				}
			}
		})
	}
}
