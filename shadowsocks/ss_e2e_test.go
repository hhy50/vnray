package shadowsocks_test

// ss 插件端到端：独立编写的 SS AEAD 客户端 → ss 入站 → 密码路由 → 隧道 →
// 真实客户端（vnray-client）→ direct 出站 → HTTP 源站。测试客户端的加密与
// 分帧全部就地实现（标准库 AEAD + 规范字节布局），与服务端实现互为独立验证。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

const e2eSSPassword = "e2e-ss-password-0123456789"

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestSSPluginEndToEnd 跑完整链路：SS 客户端 → ss 入站 → 密码路由 → 隧道 →
// 真实客户端 → direct 出站 → HTTP 源站。
func TestSSPluginEndToEnd(t *testing.T) {
	t.Parallel()
	// HTTP 源站。
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-origin"))
	}))
	defer origin.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin addr: %v", err)
	}
	originPort, _ := strconv.Atoi(portStr)

	// 数据面服务 + 真实客户端接入。
	cfg := &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: "e2e-key"},
		},
	}
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorServiceWith(cfg, sessions, nil, nil, tcp.WithMuxer(tcp.YamuxMuxer()))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	defer func() { _ = svc.Drain(context.Background()) }()

	agent, err := client.Dial(svc.Addr(), directDispatcher(), client.DialOptions{
		Auth:  client.AuthOptions{Key: "e2e-key"},
		Info:  protocol.ClientInfo{Name: "test-edge", Version: "e2e", OS: "test", Arch: "test"},
		Muxer: tcp.YamuxMuxer(),
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer agent.Close()
	go func() { _ = agent.Serve() }()

	deadline := time.Now().Add(5 * time.Second)
	for svc.Sessions().ClientCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client session never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ss 插件：启用 + 密码路由（ss 用户写进绑定表，与生产装配同一条路径）。
	st := testStore(t)
	bindings := admin.NewBindings(st)
	plugins := plugin.NewManager(st, bindings, svc.Sessions(), nil)
	bindings.Watch(func(c admin.UserChange) {
		plugins.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
	})
	if err := plugins.Start(context.Background()); err != nil {
		t.Fatalf("start plugins: %v", err)
	}
	ss, err := plugins.Get("ss")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	defer ss.Stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	freePort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := ss.Enable(context.Background(), map[string]string{"port": strconv.Itoa(freePort)}); err != nil {
		t.Fatalf("enable ss: %v", err)
	}
	if err := bindings.PutUser(store.UserBinding{
		Key: e2eSSPassword, Protocol: store.ProtocolSS, Name: "e2e-ss",
		Status: store.UserEnabled, Client: "test-edge",
	}); err != nil {
		t.Fatalf("register ss user: %v", err)
	}

	// SS 客户端走全链路：一个连接发 HTTP 请求，读到源站响应。
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(freePort))
	if err != nil {
		t.Fatalf("dial ss: %v", err)
	}
	defer conn.Close()
	sc, err := dialSSClient(conn, e2eSSPassword, host, originPort,
		fmt.Appendf(nil, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host))
	if err != nil {
		t.Fatalf("ss handshake: %v", err)
	}
	got, err := sc.readAll(time.Second * 5)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(got), "hello-from-origin") {
		t.Fatalf("response = %q, want origin payload", string(got))
	}
	for _, method := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
		if err := bindings.PutUser(store.UserBinding{
			Key: method + "-password", Protocol: store.ProtocolSS, Name: method,
			Status: store.UserEnabled, Client: "test-edge", Config: store.UserConfig{SSMethod: method},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"live", "restored"} {
		if phase == "restored" {
			ss.Stop()
			ss.Restore(context.Background())
		}
		for _, method := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
			t.Run(phase+"/"+method, func(t *testing.T) {
				conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(freePort), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				sc, err := dialSSClientMethod(conn, method+"-password", host, originPort,
					fmt.Appendf(nil, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host), method)
				if err != nil {
					t.Fatal(err)
				}
				got, err := sc.readAll(5 * time.Second)
				if err != nil || !strings.Contains(string(got), "hello-from-origin") {
					t.Fatalf("response = %q, err = %v", got, err)
				}
			})
		}
	}
}

// directDispatcher 直连出站的默认分发器。
func directDispatcher() dispatch.Dispatcher {
	return dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
}

// --- 独立实现的 SS AEAD 测试客户端 ---

// ssClient 是一条完成握手、可读响应的 SS 连接。
type ssClient struct {
	conn  net.Conn
	aead  cipher.AEAD
	nonce [12]byte
}

// ssKey 派生主密钥（EVP_BytesToKey，MD5、无盐、count=1）。
func ssKey(password []byte, keyLen int) []byte {
	sum := md5.Sum(password)
	out := append([]byte{}, sum[:]...)
	prev := sum[:]
	for len(out) < keyLen {
		h := md5.New()
		_, _ = h.Write(prev)
		_, _ = h.Write(password)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen]
}

// ssSubkey 派生会话子密钥（HKDF-SHA1，info="ss-subkey"）。
func ssSubkey(master, salt []byte, keyLen int) []byte {
	r := hkdf.New(sha1.New, master, salt, []byte("ss-subkey"))
	out := make([]byte, keyLen)
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
	return out
}

// sealChunk 按规范封装一块：[加密 2B 长度 + 16B 标签] + [加密载荷 + 16B 标签]，
// nonce 从全零起小端递增（计数器起点全 0xFF、先加一再返回，首个即全零）。
func sealChunk(a cipher.AEAD, n *[12]byte, plain []byte) []byte {
	next := func() []byte {
		for i := range n {
			n[i]++
			if n[i] != 0 {
				break
			}
		}
		return n[:]
	}
	size := make([]byte, 2)
	size[0] = byte(len(plain) >> 8)
	size[1] = byte(len(plain))
	out := a.Seal(nil, next(), size, nil)
	return a.Seal(out, next(), plain, nil)
}

// fullCounter 返回 nonce 计数器初值：全 0xFF，配合"先加一再返回"使首个 nonce 为全零。
func fullCounter() [12]byte {
	var n [12]byte
	for i := range n {
		n[i] = 0xFF
	}
	return n
}

// dialSSClient 完成 SS 握手：盐 + 首块（SOCKS5 地址段 + 应用首数据），读响应盐。
func dialSSClient(conn net.Conn, password, host string, port int, first []byte) (*ssClient, error) {
	return dialSSClientMethod(conn, password, host, port, first, "aes-256-gcm")
}

func dialSSClientMethod(conn net.Conn, password, host string, port int, first []byte, method string) (*ssClient, error) {
	keyLen := 32
	if method == "aes-128-gcm" {
		keyLen = 16
	}
	saltLen := keyLen
	newCipher := func(key []byte) (cipher.AEAD, error) {
		if method == "chacha20-ietf-poly1305" {
			return chacha20poly1305.New(key)
		}
		a, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(a)
	}
	master := ssKey([]byte(password), keyLen)
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := conn.Write(salt); err != nil {
		return nil, err
	}
	aead, err := newCipher(ssSubkey(master, salt, keyLen))
	if err != nil {
		return nil, err
	}

	n := fullCounter()
	addr := []byte{0x03, byte(len(host))}
	addr = append(addr, host...)
	addr = append(addr, byte(port>>8), byte(port))
	frame := sealChunk(aead, &n, append(addr, first...))
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}

	respSalt := make([]byte, saltLen)
	if _, err := io.ReadFull(conn, respSalt); err != nil {
		return nil, fmt.Errorf("read response salt: %w", err)
	}
	respAEAD, err := newCipher(ssSubkey(master, respSalt, keyLen))
	if err != nil {
		return nil, err
	}
	return &ssClient{conn: conn, aead: respAEAD}, nil
}

// readAll 收全部响应块，直到对端关闭（TCP FIN）；限时。
func (c *ssClient) readAll(limit time.Duration) ([]byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(limit))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	var out []byte
	n := fullCounter()
	for {
		block := make([]byte, 2+16)
		if _, err := io.ReadFull(c.conn, block); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out, nil
			}
			return nil, err
		}
		next := func() []byte {
			for i := range n {
				n[i]++
				if n[i] != 0 {
					break
				}
			}
			return n[:]
		}
		sizePlain, err := c.aead.Open(nil, next(), block, nil)
		if err != nil {
			return nil, fmt.Errorf("open length: %w", err)
		}
		size := int(sizePlain[0])<<8 | int(sizePlain[1])
		payload := make([]byte, size+16)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			return nil, err
		}
		plain, err := c.aead.Open(nil, next(), payload, nil)
		if err != nil {
			return nil, fmt.Errorf("open payload: %w", err)
		}
		out = append(out, plain...)
	}
}
