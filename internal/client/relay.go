package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/tcp"
)

// 服务端握手等待 5 秒，留出应答往返时间。
const targetDialTimeout = 4 * time.Second

type tcpDialer interface {
	DialTCP(context.Context, dispatch.Destination) (net.Conn, error)
}

func openOutbound(ctx context.Context, dispatcher dispatch.Dispatcher, dest dispatch.Destination) (io.ReadWriteCloser, error) {
	if dialer, ok := dispatcher.(tcpDialer); ok && dest.Network != "udp" {
		dialCtx, cancel := context.WithTimeout(ctx, targetDialTimeout)
		defer cancel()
		conn, err := dialer.DialTCP(dialCtx, dest)
		if err == nil && dialCtx.Err() != nil {
			_ = conn.Close()
			return nil, dialCtx.Err()
		}
		cancel()
		if !errors.Is(err, errors.ErrUnsupported) {
			return conn, err
		}
	}
	link, err := dispatcher.Dispatch(ctx, dest)
	if err != nil {
		return nil, err
	}
	return &tunnelLink{link: link}, nil
}

func writeTunnelSuccess(stream tcp.Stream) error {
	frame, err := protocol.NewReplyFrame(protocol.TunnelResult, protocol.Reply{OK: true})
	if err != nil {
		return err
	}
	return protocol.NewEncoder(stream).WriteFrame(frame)
}

func watchCancellation(ctx context.Context, abort func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		routine.Go("client.relay.cancel", func() {
			defer close(done)
			abort()
		})
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

type tunnelLink struct{ link *dispatch.Link }

// Read 读取出站响应。
func (l *tunnelLink) Read(p []byte) (int, error) { return l.link.Reader.Read(p) }

// Write 写入请求数据。
func (l *tunnelLink) Write(p []byte) (int, error) { return l.link.Writer.Write(p) }

// CloseWrite 传播上行 EOF 并保留响应方向。
func (l *tunnelLink) CloseWrite() error { return l.link.Writer.Close() }

// Close 幂等中断管道两端。
func (l *tunnelLink) Close() error {
	l.link.Reader.Interrupt()
	l.link.Writer.Interrupt()
	return nil
}

func relayStream(ctx context.Context, stream tcp.Stream, outbound io.ReadWriteCloser, udp bool) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = outbound.Close()
			// Stream.Close 只发 FIN，必须另外解除仍在等待请求的读方向。
			_ = stream.SetReadDeadline(time.Now())
			_ = stream.Close()
		})
	}
	abort := func() {
		_ = stream.SetDeadline(time.Now())
		stop()
	}
	stopWatch := watchCancellation(ctx, abort)
	defer stopWatch()

	var upstream sync.WaitGroup
	upstream.Add(1)
	routine.Go("client.relay.up", func() {
		defer upstream.Done()
		clean := false
		defer func() {
			if !clean {
				abort()
			}
		}()
		if _, err := io.Copy(outbound, stream); err != nil || udp {
			return
		}
		if err := closeWrite(outbound); err != nil {
			return
		}
		clean = true
	})
	defer upstream.Wait()
	clean := false
	defer func() {
		if clean {
			stop()
		} else {
			abort()
		}
	}()
	_, err := io.Copy(stream, outbound)
	clean = err == nil
}

func closeWrite(conn io.Closer) error {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return conn.Close()
}
