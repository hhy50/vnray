package plugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/hhy50/vnray/internal/routine"
)

// ErrDraining 表示插件已进入进程收尾，不能再修改运行配置。
var ErrDraining = errors.New("plugin: draining")

// Server 是一次监听代次的服务端契约：Serve 阻塞到该代次停止，Drain 让在途请求收尾，
// Done 在监听与全部在途连接都退出后关闭。
type Server interface {
	Serve(net.Listener) error
	Drain(context.Context) error
	Done() <-chan struct{}
}

// InboundRun 是一次监听代次：监听器、服务端与退出信号，由 Runtime 统一驱动。
type InboundRun struct {
	listener net.Listener
	server   Server
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	stopped  atomic.Bool
	err      error
}

// StartInbound 启动并登记一个监听代次。
func (r *Runtime) StartInbound(name string, ln net.Listener, build func(context.Context) Server, failed func(error)) (*InboundRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.IsDraining() {
		_ = ln.Close()
		return nil, ErrDraining
	}
	ctx, cancel := context.WithCancel(r.ctx)
	server := build(ctx)
	run := &InboundRun{listener: ln, server: server, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	r.runs.Store(run, struct{}{})
	routine.Go(name, func() {
		defer close(run.done)
		var watcher sync.WaitGroup
		watcher.Add(1)
		routine.Go(name+".cancel", func() {
			defer watcher.Done()
			select {
			case <-ctx.Done():
				run.Stop()
				run.err = server.Drain(context.Background())
			case <-server.Done():
			}
		})
		defer func() {
			_ = ln.Close()
			<-server.Done()
			cancel()
			watcher.Wait()
			r.runs.Delete(run)
		}()
		if err := server.Serve(ln); err != nil && !run.stopped.Load() && ctx.Err() == nil {
			failed(err)
		}
	})
	return run, nil
}

// Stop 关闭该代次的监听；在途连接由 DrainRuns 等待收尾。
func (r *InboundRun) Stop() {
	r.stopped.Store(true)
	_ = r.listener.Close()
}

// Done 在该监听代次及其全部连接退出后关闭。
func (r *InboundRun) Done() <-chan struct{} { return r.done }

func (r *InboundRun) drain(ctx context.Context) error {
	r.cancel()
	// 调用方的期限仍约束 HTTP 的优雅关闭及强制断连。
	err := r.server.Drain(ctx)
	select {
	case <-r.done:
		return errors.Join(err, r.err)
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

// DrainGate 是插件管理器与各运行态共用的收尾状态门。
type DrainGate struct {
	draining atomic.Bool
	ctx      context.Context
}

// IsDraining 报告管理器或插件是否已进入收尾。
func (g *DrainGate) IsDraining() bool {
	return g != nil && (g.draining.Load() || g.ctx != nil && g.ctx.Err() != nil)
}

func (g *DrainGate) begin() { g.draining.Store(true) }

// Runtime 是插件的运行态骨架：一组监听代次 + 收尾状态门 + 最近错误。
// 它被各插件实现共享，也被包外的插件实现（如 sqlite-web）复用。
type Runtime struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	gate      *DrainGate
	lastError string
	runs      sync.Map
}

// NewRuntime 创建运行态骨架，ctx 随 BeginDrain 取消。
func NewRuntime() Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	return Runtime{ctx: ctx, cancel: cancel, gate: &DrainGate{}}
}

// BindDrainGate 在插件注册时接入管理器的状态门；须在插件启动前调用。
func (r *Runtime) BindDrainGate(g *DrainGate) {
	if r.IsDraining() {
		g.begin()
	}
	if g.ctx != nil {
		r.cancel()
		r.ctx, r.cancel = context.WithCancel(g.ctx)
	}
	r.gate = g
}

// IsDraining 报告运行态是否已进入收尾。
func (r *Runtime) IsDraining() bool { return r.gate.IsDraining() || r.ctx.Err() != nil }

// Context 返回随收尾取消的上下文（插件的数据面与 HTTP 会话挂在它上面）。
func (r *Runtime) Context() context.Context { return r.ctx }

// LastError 返回最近一次操作的错误。
func (r *Runtime) LastError() string { return r.lastError }

// Record 记录本次操作的错误并原样返回，供各方法一行收尾。
func (r *Runtime) Record(err error) error {
	r.lastError = ""
	if err != nil {
		r.lastError = err.Error()
	}
	return err
}

// BeginDrain 取消运行态 ctx，并返回需要等待的监听代次。
func (r *Runtime) BeginDrain() []*InboundRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate.begin()
	if r.cancel != nil {
		r.cancel()
	}
	runs := make([]*InboundRun, 0)
	r.runs.Range(func(key, _ any) bool {
		run := key.(*InboundRun)
		runs = append(runs, run)
		return true
	})
	return runs
}

// DrainRuns 逐个等待代次退出并汇总错误。
func DrainRuns(ctx context.Context, runs []*InboundRun) error {
	var errs []error
	for _, run := range runs {
		if err := run.drain(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// HTTPInbound 把 http.Server 适配成 Server：Done 在监听循环与全部连接退出后关闭。
type HTTPInbound struct {
	server    *http.Server
	active    atomic.Int64
	serveDone atomic.Bool
	finished  atomic.Bool
	done      chan struct{}
}

// NewHTTPInbound 包装一个 http.Server，接住它的连接状态以判断何时真正收尾。
func NewHTTPInbound(server *http.Server) *HTTPInbound {
	s := &HTTPInbound{server: server, done: make(chan struct{})}
	previous := server.ConnState
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if previous != nil {
			previous(conn, state)
		}
		switch state {
		case http.StateNew:
			s.active.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.active.Add(-1)
		}
		s.finish()
	}
	return s
}

func (s *HTTPInbound) Serve(ln net.Listener) error {
	defer func() {
		s.serveDone.Store(true)
		s.finish()
	}()
	return s.server.Serve(ln)
}

func (s *HTTPInbound) finish() {
	if s.serveDone.Load() && s.active.Load() == 0 && s.finished.CompareAndSwap(false, true) {
		close(s.done)
	}
}

// Done 在监听循环与全部 HTTP 连接退出后关闭。
func (s *HTTPInbound) Done() <-chan struct{} { return s.done }

func (s *HTTPInbound) Drain(ctx context.Context) error {
	if err := s.server.Shutdown(ctx); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		_ = s.server.Close()
		return err
	}
	return nil
}
