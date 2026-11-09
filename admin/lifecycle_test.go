package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newLifecycleAdmin(t *testing.T, handler http.Handler) *AdminService {
	t.Helper()
	s := &AdminService{
		streamStop: make(chan struct{}),
		auth:       &AuthService{sessions: newSessionStore()},
	}
	s.srv = &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           s.trackRequests(handler),
		ReadHeaderTimeout: time.Second,
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Drain(ctx)
	})
	return s
}

func requestAdmin(s *AdminService) <-chan error {
	done := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + s.Addr())
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			err = errors.Join(err, resp.Body.Close())
		}
		done <- err
	}()
	return done
}

func waitAdminSignal(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestAdminDrainForceCloseWaitsForHandler(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	s := newLifecycleAdmin(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(finished)
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
	}))
	requestDone := requestAdmin(s)
	waitAdminSignal(t, entered, "request entry")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.Drain(ctx) }()
	waitAdminSignal(t, canceled, "request cancellation")
	select {
	case err := <-drained:
		t.Fatalf("drain returned before handler: %v", err)
	default:
	}
	if err := s.WaitDrained(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait active handler = %v", err)
	}
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after stopping = %d", w.Code)
	}
	releaseHandler()
	select {
	case err := <-drained:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("force close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
	waitAdminSignal(t, finished, "request completion")
	waitAdminSignal(t, s.serveDone, "serve completion")
	waitAdminSignal(t, s.authDone, "auth completion")
	if err := s.WaitDrained(ctx); err != nil {
		t.Fatalf("wait completed service = %v", err)
	}
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("request connection remained open")
	}
}

func TestAdminDrainAllowsAdmittedRequestToFinish(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	requestContext := make(chan error, 1)
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	s := newLifecycleAdmin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		requestContext <- r.Context().Err()
		_, _ = w.Write([]byte("ok"))
	}))
	requestDone := requestAdmin(s)
	waitAdminSignal(t, entered, "request entry")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.Drain(ctx) }()
	waitAdminSignal(t, s.streamStop, "drain start")
	select {
	case err := <-drained:
		t.Fatalf("drain returned before handler: %v", err)
	default:
	}
	releaseHandler()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("graceful drain = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("graceful drain did not finish")
	}
	if err := <-requestContext; err != nil {
		t.Fatalf("graceful request canceled: %v", err)
	}
	if err := <-requestDone; err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("repeated drain = %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("stopped service restarted")
	}
}

func TestAdminDrainBeforeStart(t *testing.T) {
	s := &AdminService{srv: &http.Server{}, streamStop: make(chan struct{})}
	if err := s.WaitDrained(context.Background()); err != nil {
		t.Fatalf("wait unstarted service = %v", err)
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("drain unstarted service = %v", err)
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("repeat unstarted drain = %v", err)
	}
}
