package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestClientConfigColumnsPreserveConcurrentWrites(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			ctx := context.Background()
			if err := st.SetClient(ctx, ClientBinding{Key: "edge", Type: ClientTypeLocal}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for _, write := range []func(context.Context, string, string) error{st.SetClientReportedConfig, st.SetClientServerConfig} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range 20 {
						if err := write(ctx, "edge", fmt.Sprint(i)); err != nil {
							errs <- err
							return
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			got, ok, err := st.GetClient(ctx, "edge")
			if err != nil || !ok || got.Config != "19" || got.ServerConfig != "19" || got.Type != ClientTypeLocal {
				t.Fatalf("concurrent columns lost: %+v, %v", got, err)
			}
			if err := st.SetClientReportedConfig(ctx, "new-edge", "report"); err != nil {
				t.Fatal(err)
			}
			if err := st.SetClientServerConfig(ctx, "new-edge", "settings"); err != nil {
				t.Fatal(err)
			}
			got, ok, err = st.GetClient(ctx, "new-edge")
			if err != nil || !ok || got.Config != "report" || got.ServerConfig != "settings" || got.Type != ClientTypeProxy {
				t.Fatalf("new client columns lost: %+v, %v", got, err)
			}
		})
	}
}
