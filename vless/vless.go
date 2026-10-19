package vless

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/vmess"
)

// 请求命令与地址类型字节，与 v2fly/Xray 一致。
const (
	CommandTCP = 0x01
	CommandUDP = 0x02
	CommandMux = 0x03

	addrTypeIPv4   byte = 0x01
	addrTypeDomain byte = 0x02
	addrTypeIPv6   byte = 0x03
)

// Target 是 VLESS 请求的目标地址。mux 主连接的目标是标记域名 v1.mux.cool，
// 真实目标在各 mux 子连接帧里。
type Target struct {
	Host string
	Port int
}

// String 实现 fmt.Stringer。
func (t Target) String() string { return fmt.Sprintf("%s:%d", t.Host, t.Port) }

// Handler 处理解码完成的 VLESS 连接：user 为认证通过的 UUID，client 为其路由
// 出口；conn 在响应头写出之后交付，Read/Write 即明文载荷通道。
type Handler func(user, client string, target Target, conn net.Conn)

// user 是一个已注册的 VLESS 账号。
type user struct {
	uuid   string
	client string
}

// Validator 维护 VLESS 账号：UUID 明文即凭据，认证是 O(1) 的字节比对。
type Validator struct {
	mu    sync.RWMutex
	users map[[16]byte]*user
}

// NewValidator 创建空的账号验证器。
func NewValidator() *Validator {
	return &Validator{users: make(map[[16]byte]*user)}
}

// Add 注册一个账号；uuid 必须是标准 36 字符格式（与 vmess 共用同一解析）。
func (v *Validator) Add(uuid, client string) error {
	raw, err := vmess.ParseUUID(uuid)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.users[raw] = &user{uuid: uuid, client: client}
	return nil
}

// Remove 注销一个账号。
func (v *Validator) Remove(uuid string) {
	raw, err := vmess.ParseUUID(uuid)
	if err != nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.users, raw)
}

// get 按 UUID 字节取账号。
func (v *Validator) get(raw [16]byte) (*user, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	u, ok := v.users[raw]
	return u, ok
}

// Server 是 VLESS 入站服务：解析明文请求头、回响应头，把裸连接交给 Handler。
type Server struct {
	ln        net.Listener
	validator *Validator
	handler   Handler

	mu     sync.Mutex
	closed bool
}

// NewServer 创建 VLESS 入站服务。
func NewServer(validator *Validator, handler Handler) *Server {
	return &Server{validator: validator, handler: handler}
}

// Serve 在给定监听器上接受连接并逐条处理；阻塞直到监听器关闭。
func (s *Server) Serve(ln net.Listener) error {
	s.ln = ln
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("vless: accept: %w", err)
		}
		routine.Go("vless.conn", func() {
			s.handle(conn)
		})
	}
}

// Close 停止接受新连接；已在处理的连接由各自协程收尾。
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

// headerWaitTimeout 是请求头的等待上限；VLESS 头是明文，恶意半开连接必须掐断。
const headerWaitTimeout = 15 * time.Second

// handle 驱动一条 VLESS 连接：解析明文头 → 回响应头 → 交给 Handler。
func (s *Server) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(headerWaitTimeout))
	defer func() { _ = conn.Close() }()

	// 头布局：version 1B + UUID 16B + addonsLen 1B [+ addons] + command 1B。
	var head [18]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		logging.Debugf("vless: read header failed, remote=%s", conn.RemoteAddr())
		return
	}
	if head[0] != 0x00 {
		logging.Warnf("vless: bad version %d, remote=%s", head[0], conn.RemoteAddr())
		return
	}
	var id [16]byte
	copy(id[:], head[1:17])
	u, ok := s.validator.get(id)
	if !ok {
		logging.Warnf("vless: unknown uuid, remote=%s", conn.RemoteAddr())
		return
	}
	// addons 是 protobuf flow 声明（如 xtls-rprx-vision）；本入站无法兑现，非空即拒。
	if head[17] != 0 {
		logging.Warnf("vless: flow addons not supported, remote=%s", conn.RemoteAddr())
		return
	}

	var cmd [1]byte
	if _, err := io.ReadFull(conn, cmd[:]); err != nil {
		logging.Debugf("vless: read command failed, remote=%s", conn.RemoteAddr())
		return
	}
	var target Target
	switch cmd[0] {
	case CommandTCP:
		t, err := readTarget(conn)
		if err != nil {
			logging.Warnf("vless: read target failed, remote=%s, err=%v", conn.RemoteAddr(), err)
			return
		}
		target = t
	case CommandMux:
		target = Target{Host: vmess.MuxCoolHost, Port: 0}
	default:
		logging.Warnf("vless: unsupported command %d, remote=%s", cmd[0], conn.RemoteAddr())
		return
	}

	// 响应头 2B [version, addonsLen]。v2fly 把它挂在首包响应上懒写，这里解析
	// 成功立即回：客户端两种都兼容，且与 vmess 入站"握手即应答"的行为一致。
	if _, err := conn.Write([]byte{0x00, 0x00}); err != nil {
		logging.Warnf("vless: write response header failed, remote=%s", conn.RemoteAddr())
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	logging.Infof("vless: request accepted, user=%s, client=%s, target=%s",
		u.uuid, u.client, target)
	s.handler(u.uuid, u.client, target, conn)
}

// readTarget 读 TCP 命令的端口与地址段：端口 2B → 类型 1B → 地址本体。
func readTarget(r io.Reader) (Target, error) {
	var head [3]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Target{}, err
	}
	port := int(binary.BigEndian.Uint16(head[0:2]))
	host, err := readAddress(r, head[2])
	if err != nil {
		return Target{}, err
	}
	return Target{Host: host, Port: port}, nil
}

// readAddress 解析地址本体：类型 1=IPv4 / 2=域名 / 3=IPv6。
func readAddress(r io.Reader, typ byte) (string, error) {
	switch typ {
	case addrTypeIPv4:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case addrTypeDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", err
		}
		host := make([]byte, l[0])
		if _, err := io.ReadFull(r, host); err != nil {
			return "", err
		}
		return string(host), nil
	case addrTypeIPv6:
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	default:
		return "", errors.New("vless: unknown address type")
	}
}
