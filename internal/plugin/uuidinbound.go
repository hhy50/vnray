package plugin

// uuidinbound.go — uuid 认证协议的入站在本机的运行态。vmess 与 vless 只有验证器与
// Serve 实现不同，监听、端口与启停语义完全一致，因此收成一个类型：新增第三种 uuid
// 协议时只需再给一份 serve 闭包。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/store"
)

// uuidValidator 是 uuid 认证协议的账号表；vmess.Validator 与 vless.Validator 都满足它。
type uuidValidator interface {
	Add(uuid, client string) error
	Remove(uuid string)
}

// uuidInbound 是一种 uuid 入站的运行态。并发访问由所属插件的 mu 保护，自身不加锁。
type uuidInbound struct {
	proto     store.Protocol
	validator uuidValidator
	serve     func(ln net.Listener) error
	cancel    context.CancelFunc
	listener  net.Listener
	port      int
	running   bool
}

// effectivePort 返回该入站的有效端口：已配置值；未配置时取出厂默认值。
func (in *uuidInbound) effectivePort(def int) int {
	if in.port > 0 {
		return in.port
	}
	return def
}

// start 在给定端口起监听；tlsCfg 非 nil 时在监听器外再包一层 TLS（vless 用）。
// 调用方必须持插件锁。
func (in *uuidInbound) start(port int, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("plugin: %s listen :%d: %w", in.proto, port, err)
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	// 监听器的生命周期只由 stop 管理，不随调用方 ctx 消亡：管理台的 Enable/SetPorts
	// 带的是 HTTP 请求 ctx，请求返回即取消，绑上去会把刚起的监听立刻关掉。
	runCtx, cancel := context.WithCancel(context.Background())
	in.cancel = cancel
	in.listener = ln
	in.port = port
	in.running = true
	logging.Info("v2ray: inbound started", "protocol", in.proto, "port", port, "tls", tlsCfg != nil)

	name := "plugin.v2ray." + string(in.proto)
	routine.Go(name+".watch", func() {
		<-runCtx.Done()
		_ = ln.Close()
	})
	routine.Go(name+".accept", func() {
		if err := in.serve(ln); err != nil && runCtx.Err() == nil {
			logging.Warn("v2ray: inbound stopped", "protocol", in.proto, "err", err)
		}
	})
	return nil
}

// stop 停止监听；调用方必须持插件锁。
func (in *uuidInbound) stop() {
	if !in.running {
		return
	}
	if in.cancel != nil {
		in.cancel()
	}
	if in.listener != nil {
		_ = in.listener.Close()
	}
	in.running = false
	logging.Info("v2ray: inbound stopped", "protocol", in.proto, "port", in.port)
}
