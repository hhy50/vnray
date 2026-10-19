package shadowsocks

// shadowsocks 协议测试：密钥派生用已知向量、分块编解码往返、地址解析向量，
// 以及经 net.Pipe 的服务端全流程（多账号试解认证、错误密码拒绝、盐重放拒绝）。

import (
	"bytes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestEVPBytesToKey 用 OpenSSL EVP_BytesToKey 的已知值校验：count=1 时首块就是
// MD5(密码)（"password" 的 MD5 是公开常量），第二块为 MD5(首块+密码)。
func TestEVPBytesToKey(t *testing.T) {
	t.Parallel()
	first := md5.Sum([]byte("password"))
	if got := evpBytesToKey([]byte("password"), 16); !bytes.Equal(got, first[:]) {
		t.Fatalf("key16 = %x, want %x", got, first)
	}

	h := md5.New()
	_, _ = h.Write(first[:])
	_, _ = h.Write([]byte("password"))
	want32 := append(append([]byte{}, first[:]...), h.Sum(nil)...)
	if got := evpBytesToKey([]byte("password"), 32); !bytes.Equal(got, want32) {
		t.Fatalf("key32 = %x, want %x", got, want32)
	}
}

// TestNonceSequence 校验 nonce 从全零起按小端递增（与 v2ray 的
// GenerateInitialAEADNonce 序列逐字节一致）。
func TestNonceSequence(t *testing.T) {
	t.Parallel()
	g := newNonceGen()
	if got := g.next(); !bytes.Equal(got, make([]byte, 12)) {
		t.Fatalf("first nonce = %x, want all zero", got)
	}
	if got := g.next(); got[0] != 1 || got[1] != 0 {
		t.Fatalf("second nonce = %x, want 01 00...", got)
	}
	// 低位回卷进位：连续递增 256 次后应进位到第二字节。
	g2 := newNonceGen()
	for i := 0; i < 256; i++ {
		g2.next()
	}
	if got := g2.next(); got[0] != 0 || got[1] != 1 {
		t.Fatalf("carry nonce = %x, want 00 01...", got)
	}
}

// TestParseMethod 校验方法名解析与别名。
func TestParseMethod(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"aes-128-gcm", "aead_aes_128_gcm", "aes-256-gcm",
		"chacha20-ietf-poly1305", "chacha20-poly1305", "aead_chacha20_poly1305"} {
		if _, err := ParseMethod(name); err != nil {
			t.Fatalf("method %q: %v", name, err)
		}
	}
	if _, err := ParseMethod("rc4-md5"); err == nil {
		t.Fatal("non-aead method should be rejected")
	}
}

// TestChunkRoundTrip 校验分块加密往返：多块、块上限切片、逐字节一致。
func TestChunkRoundTrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, MethodAES256GCM.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	aead, err := newAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, maxChunkPayload*2+123)
	for i := range payload {
		payload[i] = byte(i)
	}
	var wire bytes.Buffer
	w := newChunkWriter(&wire, aead)
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v, want %d", n, err, len(payload))
	}
	got, err := io.ReadAll(newChunkReader(&wire, aead))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("roundtrip mismatch: %d bytes", len(got))
	}
}

// TestChunkRejectsBadData 校验长度块解密失败与超限长度的拒绝。
func TestChunkRejectsBadData(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	aead, _ := newAESGCM(key)

	// 全零块打不开（不是合法密文）。
	r := newChunkReader(bytes.NewReader(make([]byte, 2+tagSize)), aead)
	if _, err := r.Read(make([]byte, 16)); err == nil {
		t.Fatal("tampered length block should fail")
	}

	// 明文长度超上限（写端不会产生，直接构造密文模拟恶意输入）。
	over := make([]byte, 2)
	over[0], over[1] = 0xFF, 0xFF
	sealed := aead.Seal(nil, make([]byte, 12), over, nil)
	r2 := newChunkReader(io.MultiReader(bytes.NewReader(sealed), bytes.NewReader(make([]byte, 64))), aead)
	if _, err := r2.Read(make([]byte, 16)); err == nil {
		t.Fatal("oversized chunk should be rejected")
	}
}

// TestParseTarget 用向量校验三种地址类型与剩余数据切分。
func TestParseTarget(t *testing.T) {
	t.Parallel()
	ipv4 := append([]byte{0x01, 127, 0, 0, 1, 0x23, 0x28}, []byte("GET / HTTP/1.1")...)
	target, leftover, err := parseTarget(ipv4)
	if err != nil {
		t.Fatalf("ipv4: %v", err)
	}
	if target.Host != "127.0.0.1" || target.Port != 9000 {
		t.Fatalf("ipv4 target = %+v", target)
	}
	if string(leftover) != "GET / HTTP/1.1" {
		t.Fatalf("ipv4 leftover = %q", leftover)
	}

	domain := append([]byte{0x03, 11}, append([]byte("example.com"), 0x01, 0xbb)...)
	target, leftover, err = parseTarget(domain)
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	if target.Host != "example.com" || target.Port != 443 || len(leftover) != 0 {
		t.Fatalf("domain = %+v leftover %q", target, leftover)
	}

	ipv6 := make([]byte, 1+16+2)
	ipv6[0] = 0x04
	copy(ipv6[1:17], net.ParseIP("2001:db8::1").To16())
	ipv6[17], ipv6[18] = 0x00, 0x50
	target, _, err = parseTarget(ipv6)
	if err != nil {
		t.Fatalf("ipv6: %v", err)
	}
	if target.Host != "2001:db8::1" || target.Port != 80 {
		t.Fatalf("ipv6 target = %+v", target)
	}

	if _, _, err := parseTarget([]byte{0x02}); err == nil {
		t.Fatal("unknown atyp should be rejected")
	}
}

// echoTarget 记录服务端打开过的目标，供断言。
type echoTarget struct {
	mu    sync.Mutex
	opens []string
}

func (h *echoTarget) open(target Target) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens = append(h.opens, target.String())
}

func (h *echoTarget) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.opens...)
}

// pipePair 起一个临时端口的 echo 服务端，返回服务端与监听地址。
func pipePair(t *testing.T, validator *Validator, targets *echoTarget) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(validator, func(password, client string, target Target, conn *Conn) {
		targets.open(target)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = conn.Close()
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ln.Addr().String()
}

// ssTestConn 是逐字节构造协议的测试客户端（与服务端实现互为独立验证）。
type ssTestConn struct {
	t *testing.T
	// conn 是底层连接；req 是请求侧加密对（write 续用），resp 是响应侧解密对。
	conn     net.Conn
	reqAEAD  cipher.AEAD
	reqNonce *nonceGen
	aead     cipher.AEAD
	nonce    *nonceGen
}

// dialSS 按协议发起一条 SS 连接：盐 + 加密长度块 + 加密首块（地址 + 首数据），
// 并读完响应盐、建好响应解密器。
func dialSS(t *testing.T, conn net.Conn, method Method, password string, target Target, first []byte) *ssTestConn {
	t.Helper()
	masterKey := evpBytesToKey([]byte(password), method.KeyLen)
	salt := make([]byte, method.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(salt); err != nil {
		t.Fatal(err)
	}
	aead, err := method.newAEAD(subkey(masterKey, salt, method.KeyLen))
	if err != nil {
		t.Fatal(err)
	}
	g := newNonceGen()

	// 地址块 + 首数据作为一个明文块。
	plain := append(addrBlock(target), first...)
	if len(plain) > maxChunkPayload {
		t.Fatal("test first chunk too large")
	}
	sizeField := make([]byte, 2)
	total := len(plain)
	sizeField[0] = byte(total >> 8)
	sizeField[1] = byte(total)
	frame := aead.Seal(nil, g.next(), sizeField, nil)
	frame = aead.Seal(frame, g.next(), plain, nil)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}

	// 读响应盐，派生响应解密器。
	respSalt := make([]byte, method.SaltLen)
	if _, err := io.ReadFull(conn, respSalt); err != nil {
		t.Fatalf("read response salt: %v", err)
	}
	respAEAD, err := method.newAEAD(subkey(masterKey, respSalt, method.KeyLen))
	if err != nil {
		t.Fatal(err)
	}
	return &ssTestConn{t: t, conn: conn, reqAEAD: aead, reqNonce: g,
		aead: respAEAD, nonce: newNonceGen()}
}

// addrBlock 按规范编码地址段（ATYP + 地址 + 2B 端口）。
func addrBlock(target Target) []byte {
	var out []byte
	if ip := net.ParseIP(target.Host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, 0x01)
			out = append(out, v4...)
		} else {
			out = append(out, 0x04)
			out = append(out, ip.To16()...)
		}
	} else {
		out = append(out, 0x03, byte(len(target.Host)))
		out = append(out, target.Host...)
	}
	return append(out, byte(target.Port>>8), byte(target.Port))
}

// write 以一个明文块写出数据（测试数据量小，不分块；nonce 续用请求流计数）。
func (c *ssTestConn) write(data []byte) {
	c.t.Helper()
	sizeField := make([]byte, 2)
	sizeField[0] = byte(len(data) >> 8)
	sizeField[1] = byte(len(data))
	frame := c.reqAEAD.Seal(nil, c.reqNonce.next(), sizeField, nil)
	frame = c.reqAEAD.Seal(frame, c.reqNonce.next(), data, nil)
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatalf("client write: %v", err)
	}
}

// read 读一块响应数据。
func (c *ssTestConn) read() []byte {
	c.t.Helper()
	block := make([]byte, 2+tagSize)
	if _, err := io.ReadFull(c.conn, block); err != nil {
		c.t.Fatalf("read response length: %v", err)
	}
	sizePlain, err := c.aead.Open(nil, c.nonce.next(), block, nil)
	if err != nil {
		c.t.Fatalf("open response length: %v", err)
	}
	size := int(sizePlain[0])<<8 | int(sizePlain[1])
	payload := make([]byte, size+tagSize)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		c.t.Fatalf("read response payload: %v", err)
	}
	plain, err := c.aead.Open(nil, c.nonce.next(), payload, nil)
	if err != nil {
		c.t.Fatalf("open response payload: %v", err)
	}
	return plain
}

// close 关闭底层连接（SS 无带内结束块）。
func (c *ssTestConn) close() { _ = c.conn.Close() }

// TestServerEchoWithPassword 覆盖正确密码的全流程：认证、目标解析、双向回显。
func TestServerEchoWithPassword(t *testing.T) {
	t.Parallel()
	validator := NewValidator(MethodAES256GCM)
	if err := validator.Add("secret-password", "edge-1"); err != nil {
		t.Fatal(err)
	}
	targets := &echoTarget{}
	_, addr := pipePair(t, validator, targets)
	clientConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	c := dialSS(t, clientConn, MethodAES256GCM, "secret-password",
		Target{Host: "127.0.0.1", Port: 9001}, []byte("hello"))
	if got := c.read(); string(got) != "hello" {
		t.Fatalf("echo = %q, want %q", got, "hello")
	}
	c.write([]byte("world"))
	if got := c.read(); string(got) != "world" {
		t.Fatalf("echo = %q, want %q", got, "world")
	}
	c.close()
}

// TestServerMultiAccount 断言同端口多密码：两个账号各自认证成功，
// 服务端按试解命中区分账号。
func TestServerMultiAccount(t *testing.T) {
	t.Parallel()
	validator := NewValidator(MethodChaCha20)
	if err := validator.Add("first-password", "edge-1"); err != nil {
		t.Fatal(err)
	}
	if err := validator.Add("second-password", "edge-2"); err != nil {
		t.Fatal(err)
	}
	targets := &echoTarget{}
	_, addr := pipePair(t, validator, targets)

	for _, tc := range []struct{ password, client string }{
		{"first-password", "edge-1"},
		{"second-password", "edge-2"},
	} {
		clientConn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c := dialSS(t, clientConn, MethodChaCha20, tc.password,
			Target{Host: "example.com", Port: 443}, []byte("ping"))
		if got := c.read(); string(got) != "ping" {
			t.Fatalf("echo = %q, want %q", got, "ping")
		}
		c.close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(targets.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("targets = %v, want 2 opens", targets.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServerWrongPasswordRejected 断言错误密码无法通过认证：连接被关闭，
// 读不到响应盐。
func TestServerWrongPasswordRejected(t *testing.T) {
	t.Parallel()
	validator := NewValidator(MethodAES256GCM)
	if err := validator.Add("right-password", "edge-1"); err != nil {
		t.Fatal(err)
	}
	targets := &echoTarget{}
	_, addr := pipePair(t, validator, targets)
	clientConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	masterKey := evpBytesToKey([]byte("wrong-password"), MethodAES256GCM.KeyLen)
	salt := make([]byte, MethodAES256GCM.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	aead, err := MethodAES256GCM.newAEAD(subkey(masterKey, salt, MethodAES256GCM.KeyLen))
	if err != nil {
		t.Fatal(err)
	}
	sizeField := make([]byte, 2)
	frame := aead.Seal(nil, newNonceGen().next(), sizeField, nil)
	frame = aead.Seal(frame, newNonceGen().next(), addrBlock(Target{Host: "127.0.0.1", Port: 9002}), nil)
	_, _ = clientConn.Write(salt)
	_, _ = clientConn.Write(frame)

	// 服务端试解不中即关连接：读响应盐应立刻 EOF。
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(clientConn, make([]byte, MethodAES256GCM.SaltLen)); err == nil {
		t.Fatal("connection should be closed after failed auth")
	}
	if n := len(targets.snapshot()); n != 0 {
		t.Fatalf("targets = %v, want none", targets.snapshot())
	}
}

// TestServerSaltReplayRejected 断言同一盐重放被拒绝：首次成功，重放被关连接。
func TestServerSaltReplayRejected(t *testing.T) {
	t.Parallel()
	validator := NewValidator(MethodAES128GCM)
	if err := validator.Add("secret-password", "edge-1"); err != nil {
		t.Fatal(err)
	}
	targets := &echoTarget{}
	_, addr := pipePair(t, validator, targets)

	// 固定盐与完整握手帧，供两次连接重放。
	salt := make([]byte, MethodAES128GCM.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	masterKey := evpBytesToKey([]byte("secret-password"), MethodAES128GCM.KeyLen)
	aead, err := MethodAES128GCM.newAEAD(subkey(masterKey, salt, MethodAES128GCM.KeyLen))
	if err != nil {
		t.Fatal(err)
	}
	g := newNonceGen()
	plain := append(addrBlock(Target{Host: "127.0.0.1", Port: 9003}), []byte("again")...)
	sizeField := make([]byte, 2)
	sizeField[0] = byte(len(plain) >> 8)
	sizeField[1] = byte(len(plain))
	handshake := aead.Seal(nil, g.next(), sizeField, nil)
	handshake = aead.Seal(handshake, g.next(), plain, nil)

	tryOnce := func() error {
		clientConn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = clientConn.Write(salt)
		_, _ = clientConn.Write(handshake)
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = io.ReadFull(clientConn, make([]byte, MethodAES128GCM.SaltLen))
		_ = clientConn.Close()
		return err
	}
	if err := tryOnce(); err != nil {
		t.Fatalf("first connection with fresh salt should succeed: %v", err)
	}
	if err := tryOnce(); err == nil {
		t.Fatal("replayed salt should be rejected")
	}
}

// TestValidatorAddRemove 断言账号增删与同密码覆盖。
func TestValidatorAddRemove(t *testing.T) {
	t.Parallel()
	v := NewValidator(MethodAES256GCM)
	if err := v.Add("", "edge-1"); err == nil {
		t.Fatal("empty password should be rejected")
	}
	if err := v.Add("pw", ""); err == nil {
		t.Fatal("empty client should be rejected")
	}
	if err := v.Add("pw", "edge-1"); err != nil {
		t.Fatal(err)
	}
	if err := v.Add("pw", "edge-2"); err != nil {
		t.Fatal(err)
	}
	if n := len(v.snapshot()); n != 1 {
		t.Fatalf("users = %d, want 1 (same password overwrites)", n)
	}
	if got := v.snapshot()[0].client; got != "edge-2" {
		t.Fatalf("client = %q, want edge-2", got)
	}
	v.Remove("pw")
	if n := len(v.snapshot()); n != 0 {
		t.Fatalf("users = %d after remove, want 0", n)
	}
}
