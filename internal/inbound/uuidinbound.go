package inbound

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
)

type uuidValidator interface {
	Add(uuid, client string) error
	Remove(uuid string)
}

type tlsListener struct {
	net.Listener
	config atomic.Pointer[tls.Config]
}

func (ln *tlsListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if cfg := ln.config.Load(); cfg != nil {
		return tls.Server(conn, cfg), nil
	}
	return conn, nil
}

type uuidInbound struct {
	proto     store.Protocol
	validator uuidValidator
	newServer func(context.Context) plugin.Server
	listener  *tlsListener
	run       *plugin.InboundRun
	port      int
	running   bool
}

func (in *uuidInbound) effectivePort(def int) int {
	if in.port > 0 {
		return in.port
	}
	return def
}

func (in *uuidInbound) prepare(port int, cfg *tls.Config) (*tlsListener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("plugin: %s listen :%d: %w", in.proto, port, err)
	}
	next := &tlsListener{Listener: ln}
	next.config.Store(cfg)
	return next, nil
}

func (in *uuidInbound) stop() {
	if in.run != nil {
		in.run.Stop()
	}
	in.running = false
}
