package inbound

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/mux"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/transport"
)

const maxMuxUDPTargets = 64

type muxUDPKey struct {
	target mux.Target
	direct bool
}

type muxUDPRoute struct {
	conn     *mux.Conn
	target   mux.Target
	packets  chan []byte
	ctx      context.Context
	cancel   context.CancelFunc
	activity chan<- struct{}
}

func (r *muxUDPRoute) ReadPacket() ([]byte, error) {
	select {
	case <-r.ctx.Done():
		return nil, io.EOF
	case data := <-r.packets:
		return data, nil
	}
}

func (r *muxUDPRoute) WritePacket(data []byte) error {
	err := r.conn.WritePacketFrom(r.target, data)
	if err == nil {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return err
}

func (r *muxUDPRoute) Close() error {
	r.cancel()
	return nil
}

func (r *muxUDPRoute) closed() bool {
	return r.ctx.Err() != nil
}

func (m *muxTunnel) openUDP(parent context.Context, initial mux.Target, conn *mux.Conn) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	var wg sync.WaitGroup
	defer func() {
		cancel()
		_ = conn.Close()
		wg.Wait()
		stop()
	}()
	activity := make(chan struct{}, 1)
	wg.Add(1)
	routine.Go("inbound.mux.udp.idle", func() {
		defer wg.Done()
		timer := time.NewTimer(udpIdle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cancel()
				return
			case <-activity:
				timer.Reset(udpIdle)
			}
		}
	})
	routes := make(map[muxUDPKey]*muxUDPRoute)
	open := func(target mux.Target, direct bool) (*muxUDPRoute, error) {
		key := muxUDPKey{target: target, direct: direct}
		if route := routes[key]; route != nil && !route.closed() {
			return route, nil
		}
		for k, route := range routes {
			if route.closed() {
				delete(routes, k)
			}
		}
		if len(routes) >= maxMuxUDPTargets {
			return nil, errors.New("mux: too many UDP targets")
		}
		release := func() {}
		if m.limits != nil {
			gate := m.limits.Gate(m.identity)
			if err := acquireUserGate(ctx, gate); err != nil {
				return nil, err
			}
			release = gate.Release
		}
		tunnel, err := transport.OpenTunnel(ctx, m.sess,
			dispatch.Destination{Host: target.Host, Port: target.Port, Network: "udp"},
			protocol.TunnelRequest{Origin: "vmess-mux", Network: "udp", Direct: direct})
		if err != nil {
			release()
			return nil, err
		}
		routeCtx, routeCancel := context.WithCancel(ctx)
		route := &muxUDPRoute{conn: conn, target: target, packets: make(chan []byte, 16), ctx: routeCtx, cancel: routeCancel, activity: activity}
		routes[key] = route
		opts := m.opts
		opts.Context = routeCtx
		wg.Add(1)
		routine.Go("inbound.mux.udp", func() {
			defer wg.Done()
			defer routeCancel()
			defer release()
			transport.ProxyDatagramBidirectional(route, protocol.NewDatagramConn(tunnel), pumpMux+".udp", udpIdle, opts)
		})
		return route, nil
	}
	initial.Network = "udp"
	direct, err := m.policy.check(ctx, initial.Host)
	if err == nil {
		_, err = open(initial, direct)
	}
	if err != nil {
		m.log.Info("mux: UDP target unavailable", "client", m.client, "target", initial.String(), "err", err)
		_ = conn.CloseWithError()
		return
	}
	for {
		target, data, err := conn.ReadPacketTo()
		if err != nil {
			return
		}
		select {
		case activity <- struct{}{}:
		default:
		}
		// 每个数据报重新检查，避免目标切换或规则热更新沿用旧判定。
		direct, err := m.policy.check(ctx, target.Host)
		if err != nil {
			m.log.Info("mux: UDP destination denied", "client", m.client, "target", target.String(), "err", err)
			_ = conn.CloseWithError()
			return
		}
		route, err := open(target, direct)
		if err != nil {
			m.log.Warn("mux: open UDP tunnel failed", "client", m.client, "target", target.String(), "err", err)
			_ = conn.CloseWithError()
			return
		}
		select {
		case route.packets <- data:
		case <-route.ctx.Done():
			if ctx.Err() == nil {
				_ = conn.CloseWithError()
			}
			return
		}
	}
}
