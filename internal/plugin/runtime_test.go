package plugin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustWaitGeneration(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopped generation was not reaped")
	}
}

func TestHTTPGenerationWaitsForRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "complete")
	})}
	server.SetKeepAlivesEnabled(false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime()
	run, err := runtime.StartInbound("plugin.http.test", ln, func(context.Context) Server { return NewHTTPInbound(server) }, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		finish()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := run.drain(ctx); err != nil {
			t.Error(err)
		}
	})
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + ln.Addr().String())
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not start")
	}
	run.Stop()
	select {
	case <-run.done:
		t.Fatal("in-flight HTTP request generation was reaped")
	default:
	}
	finish()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	mustWaitGeneration(t, run.done)
	runtime.runs.Range(func(_, _ any) bool {
		t.Error("completed HTTP generation remains registered")
		return true
	})
}

func TestHTTPInboundConcurrentDisconnects(t *testing.T) {
	var previousCalls atomic.Int64
	server := &http.Server{ConnState: func(net.Conn, http.ConnState) { previousCalls.Add(1) }}
	inbound := NewHTTPInbound(server)
	const count = 32
	connections := make([]net.Conn, count)
	for i := range count {
		conn, peer := net.Pipe()
		connections[i] = conn
		defer conn.Close()
		defer peer.Close()
		server.ConnState(conn, http.StateNew)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_ = inbound.Serve(ln)
	var wg sync.WaitGroup
	for _, conn := range connections[:count-1] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.ConnState(conn, http.StateClosed)
		}()
	}
	wg.Wait()
	select {
	case <-inbound.Done():
		t.Fatal("Done closed with a remaining connection")
	default:
	}
	server.ConnState(connections[count-1], http.StateHijacked)
	mustWaitGeneration(t, inbound.Done())
	if got := previousCalls.Load(); got != count*2 {
		t.Fatalf("previous ConnState calls = %d, want %d", got, count*2)
	}
}

type contextPlugin struct {
	Plugin
	name    string
	runtime Runtime
}

func (p *contextPlugin) Name() string               { return p.name }
func (p *contextPlugin) BindDrainGate(g *DrainGate) { p.runtime.BindDrainGate(g) }
func (p *contextPlugin) Drain(ctx context.Context) error {
	return DrainRuns(ctx, p.runtime.BeginDrain())
}

func TestManagerParentCancelsRetiredHTTPGeneration(t *testing.T) {
	for _, retire := range []bool{false, true} {
		name := "active"
		if retire {
			name = "retired"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &contextPlugin{name: "http", runtime: NewRuntime()}
			manager := NewRegistry(ctx, p)
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() {
				finish()
				cancel()
				wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := manager.Drain(wait); err != nil {
					t.Error(err)
				}
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			run, err := p.runtime.StartInbound("plugin.context.test", ln, func(runCtx context.Context) Server {
				server := &http.Server{
					BaseContext: func(net.Listener) context.Context { return runCtx },
					Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
						close(entered)
						<-r.Context().Done()
						close(canceled)
						<-release
					}),
				}
				return NewHTTPInbound(server)
			}, func(err error) { t.Error(err) })
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			mustWaitGeneration(t, entered)
			if retire {
				run.Stop()
				if run.ctx.Err() != nil {
					t.Fatal("hot stop canceled an active request")
				}
			}
			cancel()
			mustWaitGeneration(t, canceled)
			select {
			case <-run.Done():
				t.Fatal("generation completed before request cleanup")
			default:
			}
			wait, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = manager.Drain(wait)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Drain before request cleanup = %v", err)
			}
			finish()
			mustWaitGeneration(t, run.Done())
		})
	}
}

func TestRuntimeCancellationDoesNotCancelSibling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &contextPlugin{name: "first", runtime: NewRuntime()}
	second := &contextPlugin{name: "second", runtime: NewRuntime()}
	_ = NewRegistry(ctx, first, second)
	first.runtime.cancel()
	if ctx.Err() != nil || second.runtime.Context().Err() != nil {
		t.Fatal("plugin cancellation escaped to parent or sibling")
	}
	cancel()
	if second.runtime.Context().Err() == nil {
		t.Fatal("parent cancellation did not reach sibling")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = second.runtime.StartInbound("plugin.rejected", ln, func(context.Context) Server {
		t.Fatal("canceled runtime constructed a new server")
		return nil
	}, func(error) {})
	if !errors.Is(err, ErrDraining) {
		t.Fatalf("start after cancellation = %v", err)
	}
}
