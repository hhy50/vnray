package shadowsocks

// 入站服务：监听 Shadowsocks 连接，逐账号试解首个长度块完成认证，解析目标地址
// 后交给 Handler；Handler 拿到的 Conn 已完成响应盐写出，Read/Write 即解密/加密
// 的载荷通道。Shadowsocks 没有带内结束块，连接关闭即双向结束。

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
)

// Target 是 Shadowsocks 请求的目标地址。
type Target struct {
	Host string
	Port int
}

// String 实现 fmt.Stringer。
func (t Target) String() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Conn 是一条解码完成的 Shadowsocks 连接：Read 返回解密后的请求体，
// Write 写出的响应经分块加密；Close 关闭底层连接（无带内结束块）。
type Conn struct {
	net.Conn
	body *chunkReader
	resp *chunkWriter

	closeOnce sync.Once
}

// Read 读取解密后的请求体。
func (c *Conn) Read(p []byte) (int, error) { return c.body.Read(p) }

// Write 写出加密的响应载荷。
func (c *Conn) Write(p []byte) (int, error) { return c.resp.Write(p) }

// Close 关闭底层连接。Shadowsocks 的连接结束就是 TCP 关闭，对端以 EOF 收场。
func (c *Conn) Close() error {
	c.closeOnce.Do(func() { _ = c.Conn.Close() })
	return nil
}

// Handler 处理解码完成的 Shadowsocks 连接；password 为认证通过的账号密码，
// client 为其路由出口。Handler 返回后连接关闭。
type Handler func(password, client string, target Target, conn *Conn)

// account 是一个已注册的 Shadowsocks 账号：密码派生的主密钥与其路由出口。
type account struct {
	password  string
	client    string
	masterKey []byte
}

// saltReplayWindow 是盐重放过滤的记忆时长；窗口外重放理论上可过，但捕获重放
// 依赖会话级时序，10 分钟窗口已覆盖实际攻击面，同时避免无界增长。
const saltReplayWindow = 10 * time.Minute

// saltFilter 阻断同一会话盐在窗口内重复出现（AEAD 盐重放防护）。
type saltFilter struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	lastGC time.Time
}

func newSaltFilter() *saltFilter {
	return &saltFilter{seen: make(map[string]time.Time), lastGC: time.Now()}
}

// Check 记录并校验：窗口内重复出现返回 false。
func (f *saltFilter) Check(salt []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	key := string(salt)
	if _, ok := f.seen[key]; ok {
		return false
	}
	f.seen[key] = now
	if now.Sub(f.lastGC) > time.Minute {
		for k, at := range f.seen {
			if now.Sub(at) > saltReplayWindow {
				delete(f.seen, k)
			}
		}
		f.lastGC = now
	}
	return true
}

// Validator 维护全部 Shadowsocks 账号并完成逐账号试解认证。
type Validator struct {
	mu     sync.RWMutex
	users  []*account
	salts  *saltFilter
	method Method
}

// NewValidator 创建指定加密方法的空账号验证器。
func NewValidator(method Method) *Validator {
	return &Validator{users: make([]*account, 0), salts: newSaltFilter(), method: method}
}

// Add 注册一个账号；密码不得为空（Shadowsocks 无用户名，密码即身份）。
// 同密码重复注册按覆盖处理（改路由走更新路径）。
func (v *Validator) Add(password, client string) error {
	if password == "" {
		return errors.New("shadowsocks: password must not be empty")
	}
	if client == "" {
		return errors.New("shadowsocks: client must not be empty")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	acc := &account{password: password, client: client,
		masterKey: evpBytesToKey([]byte(password), v.method.KeyLen)}
	for i, u := range v.users {
		if u.password == password {
			v.users[i] = acc
			return nil
		}
	}
	v.users = append(v.users, acc)
	return nil
}

// Remove 注销一个账号；不存在时为空操作。
func (v *Validator) Remove(password string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, u := range v.users {
		if u.password == password {
			v.users = append(v.users[:i], v.users[i+1:]...)
			return
		}
	}
}

// Reset 清空全部账号；外部账号表整体重灌前的第一步。
func (v *Validator) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.users = v.users[:0]
}

// Method 返回验证器的加密方法。
func (v *Validator) Method() Method { return v.method }

// snapshot 返回账号切片的副本，试解循环不必持锁。
func (v *Validator) snapshot() []*account {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]*account, len(v.users))
	copy(out, v.users)
	return out
}

// handshakeTimeout 是握手（读盐 + 试解认证 + 解析地址）的等待上限。
const handshakeTimeout = 15 * time.Second

// Server 是 Shadowsocks 入站服务：持有账号验证器与连接处理器，驱动每条连接的一生。
type Server struct {
	ln        net.Listener
	validator *Validator
	handler   Handler

	mu     sync.Mutex
	closed bool
}

// NewServer 创建 Shadowsocks 入站服务。
func NewServer(validator *Validator, handler Handler) *Server {
	return &Server{validator: validator, handler: handler}
}

// Serve 在给定监听器上接受连接并逐条处理；阻塞直到监听器关闭。
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("shadowsocks: accept: %w", err)
		}
		routine.Go("shadowsocks.conn", func() {
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

// handle 驱动一条 Shadowsocks 连接：读盐 → 逐账号试解认证 → 解析地址 →
// 写响应盐 → 交给 Handler。
func (s *Server) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = conn.Close() }()

	method := s.validator.method
	salt := make([]byte, method.SaltLen)
	if _, err := io.ReadFull(conn, salt); err != nil {
		logging.Debugf("shadowsocks: read salt failed, remote=%s", conn.RemoteAddr())
		return
	}
	if !s.validator.salts.Check(salt) {
		logging.Warnf("shadowsocks: salt replayed, remote=%s", conn.RemoteAddr())
		return
	}

	// 逐账号试解首个长度块：shadowsocks 无用户名，认证靠 AEAD 打开成功与否。
	block := make([]byte, 2+tagSize)
	if _, err := io.ReadFull(conn, block); err != nil {
		logging.Debugf("shadowsocks: read length block failed, remote=%s", conn.RemoteAddr())
		return
	}
	var matched *account
	var aead cipher.AEAD
	var firstLen int
	for _, u := range s.validator.snapshot() {
		a, err := method.newAEAD(subkey(u.masterKey, salt, method.KeyLen))
		if err != nil {
			continue
		}
		plain, err := a.Open(nil, newNonceGen().next(), block, nil)
		if err != nil {
			continue
		}
		firstLen = int(binary.BigEndian.Uint16(plain))
		if firstLen == 0 || firstLen > maxChunkPayload {
			logging.Warnf("shadowsocks: bad first chunk size %d, remote=%s", firstLen, conn.RemoteAddr())
			return
		}
		matched = u
		aead = a
		break
	}
	if matched == nil {
		logging.Warnf("shadowsocks: no account matched, remote=%s", conn.RemoteAddr())
		return
	}

	// 首块载荷：解出地址段与（可能同块附带的）应用首报文；body 的 nonce 从 2 起续用。
	payload := make([]byte, firstLen+tagSize)
	if _, err := io.ReadFull(conn, payload); err != nil {
		logging.Debugf("shadowsocks: read first chunk failed, remote=%s", conn.RemoteAddr())
		return
	}
	bodyNonce := newNonceGen()
	_ = bodyNonce.next()
	plain, err := aead.Open(nil, bodyNonce.next(), payload, nil)
	if err != nil {
		logging.Warnf("shadowsocks: open first chunk failed, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}
	target, leftover, err := parseTarget(plain)
	if err != nil {
		logging.Warnf("shadowsocks: parse target failed, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}

	// 响应盐先于 Handler 写出：客户端始终先读到盐再等块，目标拨号失败时
	// 关闭连接即 EOF，协议里没有错误帧可用。
	respSalt := make([]byte, method.SaltLen)
	if _, err := rand.Read(respSalt); err != nil {
		logging.Warnf("shadowsocks: generate response salt failed, remote=%s", conn.RemoteAddr())
		return
	}
	if _, err := conn.Write(respSalt); err != nil {
		logging.Warnf("shadowsocks: write response salt failed, remote=%s", conn.RemoteAddr())
		return
	}
	respAEAD, err := method.newAEAD(subkey(matched.masterKey, respSalt, method.KeyLen))
	if err != nil {
		logging.Warnf("shadowsocks: init response cipher failed, remote=%s", conn.RemoteAddr())
		return
	}

	_ = conn.SetDeadline(time.Time{})
	sconn := &Conn{
		Conn: conn,
		body: &chunkReader{aead: aead, nonce: bodyNonce, r: conn, buf: leftover},
		resp: newChunkWriter(conn, respAEAD),
	}
	logging.Infof("shadowsocks: request accepted, user=%s, client=%s, target=%s",
		MaskPassword(matched.password), matched.client, target)
	s.handler(matched.password, matched.client, target, sconn)
}

// parseTarget 从首块明文里解析 SOCKS5 式地址段（ATYP + 地址 + 2B 端口），
// 返回目标与块内剩余数据。地址类型：0x01 IPv4 / 0x03 域名 / 0x04 IPv6，
// 与 VMess 的类型编号不同（IPv6 是 4 不是 3）。
func parseTarget(plain []byte) (Target, []byte, error) {
	if len(plain) < 1 {
		return Target{}, nil, errors.New("shadowsocks: empty address block")
	}
	atyp := plain[0]
	pos := 1
	var t Target
	switch atyp {
	case 0x01:
		if len(plain) < pos+4+2 {
			return Target{}, nil, errors.New("shadowsocks: truncated ipv4 address")
		}
		t.Host = net.IP(plain[pos : pos+4]).String()
		pos += 4
	case 0x04:
		if len(plain) < pos+16+2 {
			return Target{}, nil, errors.New("shadowsocks: truncated ipv6 address")
		}
		t.Host = net.IP(plain[pos : pos+16]).String()
		pos += 16
	case 0x03:
		if len(plain) < pos+1 {
			return Target{}, nil, errors.New("shadowsocks: truncated domain address")
		}
		dlen := int(plain[pos])
		pos++
		if dlen == 0 || len(plain) < pos+dlen+2 {
			return Target{}, nil, errors.New("shadowsocks: truncated domain address")
		}
		t.Host = string(plain[pos : pos+dlen])
		pos += dlen
	default:
		return Target{}, nil, fmt.Errorf("shadowsocks: unknown address type %#x", atyp)
	}
	t.Port = int(binary.BigEndian.Uint16(plain[pos : pos+2]))
	pos += 2
	return t, plain[pos:], nil
}

// MaskPassword 日志里不输出完整密码，只留首尾各 2 字符做辨识。
func MaskPassword(p string) string {
	if len(p) <= 4 {
		return "****"
	}
	return p[:2] + "****" + p[len(p)-2:]
}
