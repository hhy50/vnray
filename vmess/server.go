package vmess

// 入站服务：监听 VMess 连接，完成 AEAD 认证与请求头解码后交给 Handler；
// Handler 拿到的 Conn 已完成响应头发送，Read/Write 即解密/加密的载荷通道。

import (
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/identity"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/routine"
	"golang.org/x/crypto/chacha20poly1305"
)

// Conn 是一条解码完成的 VMess 连接：Read 返回解密后的请求体，Write 写出的响应
// 经分块加密；Close 会写出终结块并关闭底层连接。
type Conn struct {
	net.Conn
	body *chunkReader
	resp *chunkWriter

	closeOnce sync.Once
}

// Read 读取解密后的请求体；读到对端带内终结块（请求 FIN）返回 io.EOF。
// 写方向的终结块只由 Close 发出——请求发完不代表响应结束，这里若顺手收尾
// 写方向会把还没回来的响应当成 EOF 截断。
func (c *Conn) Read(p []byte) (int, error) { return c.body.Read(p) }

// Write 写出加密的响应载荷。
func (c *Conn) Write(p []byte) (int, error) { return c.resp.Write(p) }

// ReadPacket 读出一个完整分块的明文作为单个数据报（UDP 隧道用）；终结块返回 io.EOF。
func (c *Conn) ReadPacket() ([]byte, error) { return c.body.nextPayload() }

// WritePacket 把单个数据报封装成一个分块写出（UDP 隧道用，保持数据报边界）。
func (c *Conn) WritePacket(d []byte) error { return c.resp.WritePacket(d) }

// Close 写出 VMess 终结块（兼容 v2ray 的带内 FIN，让对端响应体 Reader 拿到 EOF）
// 并关闭底层连接。
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.resp.Close()
		_ = c.Conn.Close()
	})
	return nil
}

// Handler 处理解码完成的 VMess 连接；id 为认证通过的身份（uuid + 路由出口），
// network 是隧道数据类型（"tcp"/"udp"）。Handler 返回后连接关闭。
type Handler func(id identity.Identity, target Target, network string, conn *Conn)

// Server 是 VMess 入站服务：持有账号验证器与连接处理器，驱动每条连接的一生。
type Server struct {
	ln        net.Listener
	validator *Validator
	handler   Handler
	history   *sessionHistory

	mu     sync.Mutex
	closed bool
}

// NewServer 创建 VMess 入站服务。
func NewServer(validator *Validator, handler Handler) *Server {
	return &Server{validator: validator, handler: handler, history: newSessionHistory()}
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
			return fmt.Errorf("vmess: accept: %w", err)
		}
		routine.Go("vmess.conn", func() {
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

// handleWaitTimeout 是 VMess 握手（头两段 AEAD 解码）的等待上限。
const handleWaitTimeout = 15 * time.Second

// handle 驱动一条 VMess 连接：认证 → 解析请求头 → 发响应头 → 交给 Handler。
func (s *Server) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(handleWaitTimeout))
	defer func() { _ = conn.Close() }()

	var authID authID
	if _, err := io.ReadFull(conn, authID[:]); err != nil {
		logging.Debugf("vmess: read auth id failed, remote=%s", conn.RemoteAddr())
		return
	}
	u, err := s.validator.Match(authID)
	if err != nil {
		logging.Warnf("vmess: auth rejected, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}

	plain, err := openAEADHeader(u.cmdKey[:], authID, conn)
	if err != nil {
		logging.Warnf("vmess: open aead header failed, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}
	req, err := parseRequest(plain)
	if err != nil {
		logging.Warnf("vmess: parse request failed, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}
	// 指纹判重在选项校验之前：重放的头内容完全合法，只有指纹能识破。
	if !s.history.Check(u.raw, req.bodyKey, req.bodyIV) {
		logging.Warnf("vmess: duplicated session, possibly replay attack, remote=%s", conn.RemoteAddr())
		return
	}
	if err := req.validateOptions(); err != nil {
		logging.Warnf("vmess: bad request options, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}

	// 响应头在请求解析成功后立即发出（客户端等待它才开始读响应体）。
	respHdr, _, _ := buildResponseHeader(req)
	if _, err := conn.Write(respHdr); err != nil {
		logging.Warnf("vmess: write response header failed, remote=%s", conn.RemoteAddr())
		return
	}

	bodyAuth, bodyCodec, bodyPadding, err := chunkCodec(req, false)
	if err != nil {
		logging.Warnf("vmess: bad body framing, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}
	respAuth, respCodec, respPadding, err := chunkCodec(req, true)
	if err != nil {
		logging.Warnf("vmess: bad response framing, remote=%s, err=%v", conn.RemoteAddr(), err)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	vconn := &Conn{
		Conn: conn,
		body: newChunkReader(conn, bodyAuth, bodyCodec, bodyPadding),
		resp: newChunkWriter(conn, respAuth, respCodec, respPadding),
	}
	network := "tcp"
	if req.Command == RequestCommandUDP {
		network = "udp"
	}
	logging.Debugf("vmess: request accepted, user=%s, client=%s, target=%s, network=%s",
		u.uuid, u.client, req.Target, network)
	s.handler(identity.Identity{Key: u.uuid, Client: u.client}, req.Target, network, vconn)
}

// noOpAuth 是安全级别为 none 时的直通认证器。
type noOpAuth struct{ cipher.AEAD }

func newNoOp() cipher.AEAD { return noOpCipher{} }

type noOpCipher struct{}

func (noOpCipher) Overhead() int { return 0 }

func (noOpCipher) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(dst) < len(ciphertext) {
		dst = make([]byte, len(ciphertext))
	}
	copy(dst, ciphertext)
	return dst, nil
}

func (noOpCipher) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	return append(dst, plaintext...)
}

// NonceSize 返回标准 12 字节（nonce 被忽略，但 chunkAuth 需要合法长度）。
func (noOpCipher) NonceSize() int { return 12 }

// newChaCha 构造 ChaCha20-Poly1305（密钥必须 32 字节）。
func newChaCha(key []byte) cipher.AEAD {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err)
	}
	return a
}

// sha256Sum 计算 SHA256 摘要。
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// sha256Sum16 返回 SHA256 摘要的前 16 字节。
func sha256Sum16(b []byte) [16]byte {
	sum := sha256Sum(b)
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

// ipString 把 4/16 字节 IP 还原为文本形式。
func ipString(b []byte) string {
	if len(b) == 4 {
		return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	}
	return fmt.Sprintf("%x%x%x%x%x%x%x%x", b[0:2], b[2:4], b[4:6], b[6:8], b[8:10], b[10:12], b[12:14], b[14:16])
}
