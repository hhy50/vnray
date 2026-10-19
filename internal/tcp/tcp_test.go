package tcp

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// recvTimeout 是等待事件落地的上限；远超本机回环实际耗时，给慢 CI 留余量。
const recvTimeout = 5 * time.Second

// testMuxers 是内置后端清单：每个用例都对清单里的后端各跑一遍，确保适配层行为一致。
var testMuxers = []Muxer{YamuxMuxer()}

// eventRecorder 是测试用的 EventListener：把成对事件按序写进带缓冲 channel，供测试按序断言。
type eventRecorder struct {
	events chan string

	mu           sync.Mutex
	sess         Session // 已建立的服务端会话句柄，供测试显式关闭
	handshakeErr error   // 非 nil 时握手失败，用于验证失败路径
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{events: make(chan string, 16)}
}

// OnSessionOpen 实现非阻塞语义：记录句柄、发出事件后立即返回，
// accept 循环由事件循环同步驱动。
func (r *eventRecorder) OnSessionOpen(info *SessionInfo) error {
	r.mu.Lock()
	r.sess = info.Session
	r.mu.Unlock()
	r.events <- "session.open"
	return nil
}

func (r *eventRecorder) OnSessionClose(*SessionInfo) { r.events <- "session.close" }

func (r *eventRecorder) OnStreamOpen(*SessionInfo, Stream) error {
	r.events <- "stream.open"
	return nil
}

func (r *eventRecorder) OnStreamClose(*SessionInfo, Stream) { r.events <- "stream.close" }

// Handshake 实现握手：返回携带会话句柄的元数据（生产由认证逻辑填充），
// handshakeErr 非 nil 时握手失败。
func (r *eventRecorder) Handshake(session Session) (SessionInfo, error) {
	return SessionInfo{Session: session}, r.handshakeErr
}

// session 返回已建立的服务端会话；须先等 expect("session.open") 之后调用。
func (r *eventRecorder) session() Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess
}

// expect 按序取下一个事件并断言其名称。
func (r *eventRecorder) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.events:
		if got != want {
			t.Fatalf("event = %q, want %q", got, want)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// startServer 起一个测试用监听器（Start 即驱动事件循环），返回其地址；收尾由 t.Cleanup 负责。
func startServer(t *testing.T, rec EventListener, muxer Muxer) string {
	t.Helper()

	srv := NewServer("127.0.0.1:0", rec, WithMuxer(muxer))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv.Addr()
}

// dialClient 连接监听器并用指定后端建立一条客户端会话。
func dialClient(t *testing.T, muxer Muxer, addr string) Session {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	client, err := muxer.Client(conn, DefaultSessionConfig())
	if err != nil {
		_ = conn.Close()
		t.Fatalf("create %s client: %v", muxer.Name(), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestServerDispatchesSessionAndStreamEvents 验证会话/流两级事件的成对派发；会话关闭显式触发，不依赖对端断开。
func TestServerDispatchesSessionAndStreamEvents(t *testing.T) {
	for _, muxer := range testMuxers {
		t.Run(muxer.Name(), func(t *testing.T) {
			t.Parallel()

			rec := newEventRecorder()
			client := dialClient(t, muxer, startServer(t, rec, muxer))

			rec.expect(t, "session.open")

			stream, err := client.OpenStream()
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			rec.expect(t, "stream.open")
			_ = stream.Close()
			rec.expect(t, "stream.close")

			// 显式关闭服务端会话：OnSessionOpen 随之返回并触发 OnSessionClose。
			if err := rec.session().Close(); err != nil {
				t.Fatalf("close server session: %v", err)
			}
			rec.expect(t, "session.close")
		})
	}
}

// TestNoStreamOpenAfterSessionClose 验证会话关闭后不再派发 stream.open：
// AcceptStream 的 select 在 die 与待接受队列都就绪时随机选择，可能在关闭瞬间
// 仍送出余流，防护必须把它拦下（成对事件语义不允许 close 之后出现 open）。
func TestNoStreamOpenAfterSessionClose(t *testing.T) {
	for _, muxer := range testMuxers {
		t.Run(muxer.Name(), func(t *testing.T) {
			t.Parallel()

			rec := newEventRecorder()
			client := dialClient(t, muxer, startServer(t, rec, muxer))
			rec.expect(t, "session.open")

			// 连开几条流后立刻关服务端会话：制造"die 已关、chAccepts 仍有余流"的窗口。
			const total = 3
			for i := 0; i < total; i++ {
				if _, err := client.OpenStream(); err != nil {
					t.Fatalf("open stream %d: %v", i, err)
				}
			}
			if err := rec.session().Close(); err != nil {
				t.Fatalf("close server session: %v", err)
			}

			// 排空到 session.close；此前的 stream.open/close 与关闭事件在 channel 里的
			// 顺序不确定（不同协程入队），只统计 open 数。
			opens := 0
			for {
				select {
				case ev := <-rec.events:
					switch ev {
					case "session.close":
						goto settled
					case "stream.open":
						opens++
					}
				case <-time.After(recvTimeout):
					t.Fatal("no session.close event")
				}
			}
		settled:
			// 有防护后，关闭后不应再出现任何 stream.open（已派发流的成对 stream.close 允许排空）。
			deadline := time.After(300 * time.Millisecond)
			for {
				select {
				case ev := <-rec.events:
					if ev == "stream.open" {
						t.Fatal("stream.open dispatched after session close")
					}
				case <-deadline:
					return
				}
			}
		})
	}
}

// TestHandshakeFailureSkipsEvents 验证握手失败时不派发任何事件：会话既不登记也不上报，
// 只被关闭（失败原因由 Handshake 自己返回给调用方）。
func TestHandshakeFailureSkipsEvents(t *testing.T) {
	for _, muxer := range testMuxers {
		t.Run(muxer.Name(), func(t *testing.T) {
			t.Parallel()

			rec := newEventRecorder()
			rec.handshakeErr = errors.New("handshake refused")
			dialClient(t, muxer, startServer(t, rec, muxer))

			select {
			case ev := <-rec.events:
				t.Fatalf("unexpected event %q after failed handshake", ev)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// TestStartTwice 验证重复 Start 返回错误（监听已建立，事件循环已由第一次 Start 驱动）。
func TestStartTwice(t *testing.T) {
	t.Parallel()

	srv := NewServer("127.0.0.1:0", newEventRecorder())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("second Start should return an error")
	}
}
