package transport

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/concurrency"
)

// fakeStream 是最小的 Stream 假实现，满足 gateStream 的 Close 语义测试。
type fakeStream struct {
	closed atomic.Bool
}

func (f *fakeStream) Close() error { f.closed.Store(true); return nil }

func (f *fakeStream) Read([]byte) (int, error)         { panic("unused") }
func (f *fakeStream) Write([]byte) (int, error)        { panic("unused") }
func (f *fakeStream) LocalAddr() net.Addr              { panic("unused") }
func (f *fakeStream) RemoteAddr() net.Addr             { panic("unused") }
func (f *fakeStream) SetDeadline(time.Time) error      { return nil }
func (f *fakeStream) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeStream) SetWriteDeadline(time.Time) error { return nil }
func (f *fakeStream) ID() uint32                       { return 0 }

func TestGateStreamReleasesOnce(t *testing.T) {
	gate := concurrency.NewGate()
	gate.SetMax(1)
	if err := gate.Acquire(nil); err != nil {
		t.Fatal(err)
	}
	var releases atomic.Int32
	gs := &gateStream{Stream: &fakeStream{}, release: func() {
		releases.Add(1)
		gate.Release()
	}}
	// Close 幂等：多次关闭只归还一次并发位。
	for i := 0; i < 3; i++ {
		if err := gs.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if releases.Load() != 1 {
		t.Fatalf("releases = %d, want 1", releases.Load())
	}
	if gate.Usage() != 0 {
		t.Fatalf("usage = %d, want 0", gate.Usage())
	}
}
