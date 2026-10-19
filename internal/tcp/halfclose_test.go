package tcp

// 半关闭语义的适配层定向测试：服务端 Close（up EOF）之后，下载方向的数据
// 必须全部可读——包括对端随后发来又立刻 FIN（teardown）时仍驻留在缓冲里的数据。
// smux 上游在此场景丢数据（tryHalfCloseCleanup 双 FIN 回收截断缓冲，历史补丁见
// ADR 0021/0023），yamux 原生不丢；本测试锁住该语义，任何后端接入都必须先过它。

import (
	"io"
	"net"
	"testing"
	"time"
)

// sessionPair 用一对回环连接建立 server/client 会话，收尾由 t.Cleanup 负责。
func sessionPair(t *testing.T, muxer Muxer) (server, client Session) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		s   Session
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- accepted{err: err}
			return
		}
		s, err := muxer.Server(conn, DefaultSessionConfig())
		ch <- accepted{s: s, err: err}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cli, err := muxer.Client(conn, DefaultSessionConfig())
	if err != nil {
		_ = conn.Close()
		t.Fatalf("create %s client: %v", muxer.Name(), err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	r := <-ch
	if r.err != nil {
		t.Fatalf("accept %s session: %v", muxer.Name(), r.err)
	}
	t.Cleanup(func() { _ = r.s.Close() })
	return r.s, cli
}

// TestHalfClosePreservesBufferedDataAfterPeerFin 分两个阶段锁住半关闭语义：
//
//   - "缓冲中的数据"：对端把一小段数据（小于流控窗口，必然整体驻留接收缓冲）
//     写完后立即 Close 发 FIN；服务端先 Close 再等 FIN 落定，然后才读——
//     必须读全数据再 EOF。smux 上游在此场景丢数据（双 FIN 触发
//     tryHalfCloseCleanup → recycleTokens 截断缓冲，历史见 ADR 0021/0023）。
//   - "窗口阻塞的长流"：对端写远超窗口的数据（写方会被流控阻塞直到服务端读），
//     服务端 Close 之后一边读一边收完全部数据；对端写完 Close 后读到 EOF。
func TestHalfClosePreservesBufferedDataAfterPeerFin(t *testing.T) {
	for _, muxer := range testMuxers {
		t.Run(muxer.Name(), func(t *testing.T) {
			t.Parallel()
			server, cli := sessionPair(t, muxer)

			// 阶段一：小载荷，完整驻留缓冲。
			cs, err := cli.OpenStream()
			if err != nil {
				t.Fatalf("open client stream: %v", err)
			}
			ss, err := server.AcceptStream()
			if err != nil {
				t.Fatalf("accept server stream: %v", err)
			}

			// 客户端先读到 EOF，验证服务端的 Close 真的把 FIN 送到了对端。
			clientEOF := make(chan error, 1)
			go func() {
				buf := make([]byte, 1)
				_, err := cs.Read(buf)
				clientEOF <- err
			}()
			if err := ss.Close(); err != nil {
				t.Fatalf("server close: %v", err)
			}
			select {
			case err := <-clientEOF:
				if err != io.EOF {
					t.Fatalf("client read after peer close = %v, want EOF", err)
				}
			case <-time.After(recvTimeout):
				t.Fatal("client did not observe FIN from server close")
			}

			// 对端写一小段数据后立即 Close（FIN）：数据必然整体驻留服务端缓冲。
			const small = 128 << 10
			payload := make([]byte, small)
			for i := range payload {
				payload[i] = byte(i)
			}
			if _, err := cs.Write(payload); err != nil {
				t.Fatalf("client write: %v", err)
			}
			if err := cs.Close(); err != nil {
				t.Fatalf("client close: %v", err)
			}
			// 等数据帧与 FIN 帧都在服务端落定再开始读：制造"写方向已关 + 缓冲非空
			// + 收到对端 FIN"的竞态窗口。
			time.Sleep(200 * time.Millisecond)

			got, err := io.ReadAll(ss)
			if err != nil {
				t.Fatalf("server read after peer FIN: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("server read %d bytes after peer FIN, want %d (buffered data lost)", len(got), len(payload))
			}

			// 阶段二：窗口阻塞的长流——半关闭后下载继续，对端 FIN 后仍读完全部。
			cs2, err := cli.OpenStream()
			if err != nil {
				t.Fatalf("open client stream 2: %v", err)
			}
			ss2, err := server.AcceptStream()
			if err != nil {
				t.Fatalf("accept server stream 2: %v", err)
			}
			if err := ss2.Close(); err != nil {
				t.Fatalf("server close 2: %v", err)
			}
			const large = 4 << 20 // 超过 256KiB 流控窗口，写方会被阻塞直到服务端读
			big := make([]byte, large)
			for i := range big {
				big[i] = byte(i*31 + i/251)
			}
			writeDone := make(chan error, 1)
			go func() {
				_, err := cs2.Write(big)
				if err == nil {
					err = cs2.Close()
				}
				writeDone <- err
			}()
			got2, err := io.ReadAll(ss2)
			if err != nil {
				t.Fatalf("server read long stream: %v", err)
			}
			if err := <-writeDone; err != nil {
				t.Fatalf("client write long stream: %v", err)
			}
			if string(got2) != string(big) {
				t.Fatalf("server read %d bytes of long stream, want %d", len(got2), len(big))
			}
		})
	}
}

// TestAcceptStreamDeadline 验证会话级 SetDeadline 只约束 AcceptStream 的等待：
// 期限内开到流则正常返回，期限过后返回错误，清零后恢复阻塞语义。
func TestAcceptStreamDeadline(t *testing.T) {
	for _, muxer := range testMuxers {
		t.Run(muxer.Name(), func(t *testing.T) {
			t.Parallel()
			server, cli := sessionPair(t, muxer)
			defer func() { _ = cli.Close() }()

			// 期限已过：AcceptStream 应立即失败而不是挂住。
			if err := server.SetDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}
			if _, err := server.AcceptStream(); err == nil {
				t.Fatal("AcceptStream should fail after the deadline passed")
			}

			// 清零后恢复：对端开流即可接到。
			if err := server.SetDeadline(time.Time{}); err != nil {
				t.Fatalf("clear deadline: %v", err)
			}
			cs, err := cli.OpenStream()
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			ss, err := server.AcceptStream()
			if err != nil {
				t.Fatalf("accept after clearing deadline: %v", err)
			}
			if ss.ID() != cs.ID() {
				t.Fatalf("stream id = %d, want %d", ss.ID(), cs.ID())
			}
		})
	}
}
