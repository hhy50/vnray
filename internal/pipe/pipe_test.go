package pipe

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
)

func TestPipeCloseDrainsBuffer(t *testing.T) {
	wantErr := errors.New("outbound failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "EOF"},
		{name: "error", err: wantErr},
		{name: "closed-pipe-error", err: io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := New()
			want := []byte("tail data")
			if _, err := w.Write(want); err != nil {
				t.Fatal(err)
			}
			if err := w.CloseWithError(tc.err); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(r, got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("tail = %q, %v", got, err)
			}
			terminal := tc.err
			if terminal == nil {
				terminal = io.EOF
			}
			for range 2 {
				if n, err := r.Read(got); n != 0 || err != terminal {
					t.Fatalf("read = %d, %v; want 0, %v", n, err, terminal)
				}
			}
			if n, err := w.Write(want); n != 0 || err != io.ErrClosedPipe {
				t.Fatalf("write after close = %d, %v", n, err)
			}
		})
	}
}

func TestPipeFirstCloseWins(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "interrupt"}[interrupt], func(t *testing.T) {
			r, w := New()
			if _, err := w.Write([]byte("tail")); err != nil {
				t.Fatal(err)
			}
			wantErr := errors.New("first close")
			if interrupt {
				r.Interrupt()
				wantErr = io.ErrClosedPipe
			}
			_ = w.CloseWithError(wantErr)
			_ = w.Close()
			w.Interrupt()
			r.Interrupt()
			got, err := io.ReadAll(r)
			want := "tail"
			if interrupt {
				want = ""
			}
			if string(got) != want || err != wantErr {
				t.Fatalf("read all = %q, %v; want %q, %v", got, err, want, wantErr)
			}
		})
	}
}

func TestPipeCloseUnblocksReaders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(*Reader, *Writer)
		err   error
	}{
		{name: "close", close: func(_ *Reader, w *Writer) { _ = w.Close() }, err: io.EOF},
		{name: "reader-interrupt", close: func(r *Reader, _ *Writer) { r.Interrupt() }, err: io.ErrClosedPipe},
		{name: "writer-interrupt", close: func(_ *Reader, w *Writer) { w.Interrupt() }, err: io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, w := New()
				results := make(chan error, 4)
				for range cap(results) {
					go func() {
						_, err := r.Read(make([]byte, 1))
						results <- err
					}()
				}
				synctest.Wait()
				if len(results) != 0 {
					t.Fatal("empty pipe did not block readers")
				}
				tc.close(r, w)
				for range cap(results) {
					if err := <-results; err != tc.err {
						t.Fatalf("read error = %v, want %v", err, tc.err)
					}
				}
			})
		})
	}
}

func TestPipeBoundedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := New()
		defer r.Interrupt()
		want := bytes.Repeat([]byte{42}, pipeLimit+1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if n, err := w.Write(want); n != len(want) || err != nil {
				t.Errorf("write = %d, %v", n, err)
			}
			_ = w.Close()
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("write beyond capacity completed without a reader")
		default:
		}
		got, err := io.ReadAll(r)
		<-done
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read length = %d, error = %v", len(got), err)
		}
	})
}

func TestPipeCloseUnblocksPartialWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(*Reader, *Writer)
		drain bool
	}{
		{name: "close", close: func(_ *Reader, w *Writer) { _ = w.Close() }, drain: true},
		{name: "reader-interrupt", close: func(r *Reader, _ *Writer) { r.Interrupt() }},
		{name: "writer-interrupt", close: func(_ *Reader, w *Writer) { w.Interrupt() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, w := New()
				want := bytes.Repeat([]byte{7}, pipeLimit+1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					if n, err := w.Write(want); n != pipeLimit || err != io.ErrClosedPipe {
						t.Errorf("partial write = %d, %v", n, err)
					}
				}()
				synctest.Wait()
				tc.close(r, w)
				<-done
				got, err := io.ReadAll(r)
				if tc.drain {
					if err != nil || !bytes.Equal(got, want[:pipeLimit]) {
						t.Fatalf("drain = %d bytes, %v", len(got), err)
					}
				} else if len(got) != 0 || err != io.ErrClosedPipe {
					t.Fatalf("interrupt = %d bytes, %v", len(got), err)
				}
			})
		})
	}
}

func TestPipeConcurrentWritesDoNotInterleave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := New()
		defer r.Interrupt()
		const writers = 4
		const size = 2*pipeLimit + 17
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if n, err := w.Write(bytes.Repeat([]byte{byte(i)}, size)); n != size || err != nil {
					t.Errorf("writer %d: %d, %v", i, n, err)
				}
			}()
		}
		go func() { wg.Wait(); _ = w.Close() }()
		got, err := io.ReadAll(r)
		if err != nil || len(got) != writers*size {
			t.Fatalf("read length = %d, error = %v", len(got), err)
		}
		seen := make(map[byte]bool)
		for offset := 0; offset < len(got); offset += size {
			id := got[offset]
			if seen[id] || !bytes.Equal(got[offset:offset+size], bytes.Repeat([]byte{id}, size)) {
				t.Fatal("concurrent writes interleaved or duplicated")
			}
			seen[id] = true
		}
	})
}

func TestPipeWraparoundAndInputOwnership(t *testing.T) {
	r, w := New()
	defer r.Interrupt()
	for i := range 20 {
		want := bytes.Repeat([]byte{byte(i)}, 19001)
		input := bytes.Clone(want)
		if n, err := w.Write(input); n != len(input) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
		clear(input)
		got := make([]byte, len(want))
		if _, err := io.ReadFull(r, got[:1234]); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, got[1234:]); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("wrapped read %d: error = %v, data matches = %v", i, err, bytes.Equal(got, want))
		}
	}
}

func TestPipeEmptyOperationsDoNotBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := New()
		if n, err := r.Read(nil); n != 0 || err != nil {
			t.Fatalf("empty read = %d, %v", n, err)
		}
		if n, err := w.Write(nil); n != 0 || err != nil {
			t.Fatalf("empty write = %d, %v", n, err)
		}
		_ = w.Close()
		if n, err := w.Write(nil); n != 0 || err != io.ErrClosedPipe {
			t.Fatalf("closed empty write = %d, %v", n, err)
		}
	})
}

func TestBufferObserverDeltas(t *testing.T) {
	var mu sync.Mutex
	var buffered int64
	SetBufferObserver(func(delta int64) {
		mu.Lock()
		buffered += delta
		mu.Unlock()
	})
	t.Cleanup(func() { bufferObserver.Store(nil) })

	r, w := New()
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	mu.Lock()
	if buffered != 5 {
		mu.Unlock()
		t.Fatalf("buffered after write = %d, want 5", buffered)
	}
	mu.Unlock()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	mu.Lock()
	if buffered != 0 {
		mu.Unlock()
		t.Fatalf("buffered after read = %d, want 0", buffered)
	}
	mu.Unlock()
}

func TestBufferObserverDiscardOnInterrupt(t *testing.T) {
	var buffered int64
	var mu sync.Mutex
	SetBufferObserver(func(delta int64) {
		mu.Lock()
		buffered += delta
		mu.Unlock()
	})
	t.Cleanup(func() { bufferObserver.Store(nil) })

	r, w := New()
	_, _ = w.Write([]byte("residual"))
	w.Interrupt()

	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != io.ErrClosedPipe {
		t.Fatalf("read after interrupt = %v, want io.ErrClosedPipe", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if buffered != 0 {
		t.Fatalf("buffered after interrupt = %d, want 0 (discarded bytes decremented)", buffered)
	}
}

func TestNoObserverIsNoop(t *testing.T) {
	bufferObserver.Store(nil)
	r, w := New()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestBufferAllocationIsLazyAndReused(t *testing.T) {
	before, bytesBefore := BufferAllocations()
	r, w := New()
	_ = w.Close()
	if after, _ := BufferAllocations(); after != before {
		t.Fatal("empty pipe allocated a buffer")
	}
	r, w = New()
	for range 2 {
		if _, err := w.Write([]byte("abc")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, make([]byte, 3)); err != nil {
			t.Fatal(err)
		}
	}
	w.Interrupt()
	after, bytesAfter := BufferAllocations()
	if after-before != 1 || bytesAfter-bytesBefore != pipeLimit {
		t.Fatalf("allocations = %d, bytes = %d", after-before, bytesAfter-bytesBefore)
	}
}
