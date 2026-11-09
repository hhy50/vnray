package tcp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestOpenStreamContextCancelsFullBacklog(t *testing.T) {
	a, b := net.Pipe()
	cfg := DefaultSessionConfig()
	cfg.AcceptBacklog = 1
	client, err := YamuxMuxer().Client(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := YamuxMuxer().Server(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	first, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := OpenStreamContext(ctx, client); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backlog cancellation error=%v", err)
	}
	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = accepted.Close()
	opened := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stream, err := OpenStreamContext(ctx, client)
		if err == nil {
			_, err = stream.Write([]byte("next"))
			_ = stream.Close()
		}
		opened <- err
	}()
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		payload, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) == 0 {
			continue
		}
		if string(payload) != "next" {
			t.Fatalf("payload=%q", payload)
		}
		break
	}
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenStream(); err == nil {
		t.Fatal("closed session accepted a new stream")
	}
}

func TestCloseRacesFirstOpenStream(t *testing.T) {
	for range 32 {
		a, b := net.Pipe()
		client, err := YamuxMuxer().Client(a, DefaultSessionConfig())
		if err != nil {
			t.Fatal(err)
		}
		server, err := YamuxMuxer().Server(b, DefaultSessionConfig())
		if err != nil {
			_ = client.Close()
			t.Fatal(err)
		}
		start := make(chan struct{})
		finished := make(chan struct{}, 2)
		go func() {
			<-start
			stream, err := client.OpenStream()
			if err == nil {
				_ = stream.Close()
			}
			finished <- struct{}{}
		}()
		go func() {
			<-start
			_ = client.Close()
			finished <- struct{}{}
		}()
		close(start)
		for range 2 {
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				_ = a.Close()
				_ = b.Close()
				t.Fatal("first open and close did not finish")
			}
		}
		if _, err := client.OpenStream(); err == nil {
			t.Fatal("closed session accepted a new stream")
		}
		_ = server.Close()
	}
}

func TestOpenStreamContextConcurrent(t *testing.T) {
	server, client := sessionPair(t, YamuxMuxer())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const count = 12
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := OpenStreamContext(ctx, client)
			if err == nil {
				_, err = stream.Write([]byte("x"))
				_ = stream.Close()
			}
			errs <- err
		}()
	}
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < count; i++ {
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		data, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil || string(data) != "x" {
			t.Fatalf("data=%q error=%v", data, err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
