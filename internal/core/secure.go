package core

import (
	"bufio"
	"context"
	"net"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/secure"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (s *ConnectorService) upgradeConnection(ctx context.Context, raw net.Conn) (net.Conn, error) {
	if err := raw.SetReadDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return nil, err
	}
	stopped := make(chan struct{})
	finished := make(chan struct{})
	routine.Go("core.handshake.cancel", func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-finished:
		}
	})
	r := bufio.NewReader(raw)
	first, err := r.Peek(1)
	close(finished)
	<-stopped
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	conn := &bufferedConn{Conn: raw, reader: r}
	// Legacy yamux users remain supported; Auth on this path is always rejected.
	if first[0] == 0 {
		if err := raw.SetReadDeadline(time.Time{}); err != nil {
			return nil, err
		}
		return conn, nil
	}
	return secure.Server(ctx, conn, s.lookupTransportKey, s.transportEncryption.Load())
}

func (s *ConnectorService) lookupTransportKey(ctx context.Context, id authkey.ID) (authkey.Key, error) {
	if s.sessionCfg != nil {
		key, configured, err := s.sessionCfg.LookupTransportKey(ctx, id)
		if err != nil || configured {
			return key, err
		}
	}
	key, err := authkey.Parse(s.authKey)
	if err != nil || key.ID() != id {
		return authkey.Key{}, secure.ErrHandshake
	}
	return key, nil
}
