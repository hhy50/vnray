package vmess_test

// v2ray mux 端到端：vmess mux 主连接客户端 → v2ray 入站 → mux 服务端 →
// 每条子连接各开一条隧道 → 真实客户端（vnray-client）→ direct 出站 → HTTP 源站。
// mux 客户端逐字节构造帧，与服务端实现互为独立验证。

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

// TestV2RayPluginMuxEndToEnd 跑完整 mux 链路：一条 VMess 主连接上复用多条
// 子连接，每条各走隧道到 HTTP 源站。
func TestV2RayPluginMuxEndToEnd(t *testing.T) {
	t.Parallel()
	runV2RayPluginMuxEndToEnd(t, tcp.YamuxMuxer())
}

func runV2RayPluginMuxEndToEnd(t *testing.T, muxer tcp.Muxer) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-origin"))
	}))
	defer origin.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin addr: %v", err)
	}
	originPort, _ := strconv.Atoi(portStr)

	cfg := &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: "e2e-key"},
		},
	}
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorServiceWith(cfg, sessions, nil, nil, tcp.WithMuxer(muxer))
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	defer func() { _ = svc.Drain(context.Background()) }()

	agent, err := client.Dial(svc.Addr(), directDispatcher(), client.DialOptions{
		Auth:  client.AuthOptions{Key: "e2e-key"},
		Info:  protocol.ClientInfo{Name: "test-edge", Version: "e2e", OS: "test", Arch: "test"},
		Muxer: muxer,
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

	// v2ray 插件：启用 + uuid 路由（uuid 用户写进绑定表，与生产装配同一条路径）。
	st := testStore(t)
	bindings := admin.NewBindings(st)
	plugins := plugin.NewManager(st, bindings, svc.Sessions(), nil)
	bindings.Watch(func(c admin.UserChange) {
		p, err := plugins.Get("v2ray")
		if err != nil {
			return
		}
		if syncer, ok := p.(plugin.UserSyncer); ok {
			syncer.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
		}
	})
	if err := plugins.Start(context.Background()); err != nil {
		t.Fatalf("start plugins: %v", err)
	}
	v2ray, err := plugins.Get("v2ray")
	if err != nil {
		t.Fatalf("get plugin: %v", err)
	}
	vmessPort, vlessPort := freePort(t), freePort(t)
	if err := v2ray.Enable(context.Background(), map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable v2ray: %v", err)
	}
	if err := bindings.PutUser(store.UserBinding{
		Key: e2eUUID, Protocol: store.ProtocolVMess, Name: "e2e-vmess-mux",
		Status: store.UserEnabled, Client: "test-edge",
	}); err != nil {
		t.Fatalf("register uuid: %v", err)
	}

	// mux 主连接：VMess 请求的目标是标记域名 v1.mux.cool、命令为 Mux。
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(vmessPort))
	if err != nil {
		t.Fatalf("dial v2ray: %v", err)
	}
	defer conn.Close()
	sess, err := vmess.NewMuxClientSession(e2eUUID, conn,
		vmess.SecurityAES128GCM, vmess.OptionChunkStream|vmess.OptionChunkMasking)
	if err != nil {
		t.Fatalf("vmess mux handshake: %v", err)
	}
	if err := sess.ReadResponseHeader(); err != nil {
		t.Fatalf("read response header: %v", err)
	}

	mc := &vmessMuxClient{sess: sess}
	// 两条子连接先后复用同一条主连接（v2ray 客户端的典型用法）。
	for i, marker := range []string{"first", "second"} {
		id := mc.open(host, originPort, muxRequest(marker, host))
		got, err := mc.readSubConnection(id)
		if err != nil {
			t.Fatalf("sub-connection %d: %v", i, err)
		}
		if !strings.Contains(got, "hello-from-origin") {
			t.Fatalf("sub-connection %d response = %q, want origin payload", i, got)
		}
	}
	mc.keepalive()
}

// muxRequest 构造发往源站的 HTTP 请求（Connection: close 让源站回完即关，
// 子连接随之拿到 End）。
func muxRequest(marker, host string) []byte {
	return []byte("GET / HTTP/1.1\r\nHost: " + host +
		"\r\nX-Marker: " + marker + "\r\nConnection: close\r\n\r\n")
}

// vmessMuxClient 是在 VMess 会话载荷上跑 mux.cool 协议的最小客户端：
// 逐字节构造帧、逐字节解析帧，与服务端实现互为独立验证。
type vmessMuxClient struct {
	sess *vmess.ClientSession
	wmu  sync.Mutex
	next uint16
	werr error
}

// open 开子连接：发 New 帧（首数据 = 应用的第一个报文），返回会话 ID。
func (c *vmessMuxClient) open(host string, port int, first []byte) uint16 {
	c.next++
	id := c.next
	c.write(newFrame(id, statusNew, optionData, host, port, first))
	return id
}

// keepalive 发 KeepAlive 帧（服务端应忽略）。
func (c *vmessMuxClient) keepalive() {
	c.write(newFrame(0xbeef, statusKeepAlive, 0, "", 0, []byte("junk")))
}

// readSubConnection 收一个子连接的响应：Keep 帧数据直到 End 帧。
func (c *vmessMuxClient) readSubConnection(id uint16) (string, error) {
	var got []byte
	for {
		status, option, data, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch status {
		case statusKeep:
			if option&optionData != 0 {
				got = append(got, data...)
			}
		case statusEnd:
			return string(got), nil
		case statusKeepAlive:
			// 忽略
		default:
			return "", fmt.Errorf("unexpected frame status %d", status)
		}
	}
}

func (c *vmessMuxClient) write(frame []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return
	}
	if _, err := c.sess.BodyWriter().Write(frame); err != nil {
		c.werr = err
	}
}

// readFrame 从 VMess 响应体逐字节解析一个 mux 帧。
func (c *vmessMuxClient) readFrame() (status, option byte, data []byte, err error) {
	if c.werr != nil {
		err = c.werr
		return
	}
	r := c.sess.BodyReader()
	var lenBuf [2]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return
	}
	metaLen := binary.BigEndian.Uint16(lenBuf[:])
	meta := make([]byte, metaLen)
	if _, err = io.ReadFull(r, meta); err != nil {
		return
	}
	if len(meta) < 4 {
		err = fmt.Errorf("mux client: short metadata")
		return
	}
	status, option = meta[2], meta[3]
	if option&optionData != 0 {
		var dataLen [2]byte
		if _, err = io.ReadFull(r, dataLen[:]); err != nil {
			return
		}
		data = make([]byte, binary.BigEndian.Uint16(dataLen[:]))
		if _, err = io.ReadFull(r, data); err != nil {
			return
		}
	}
	return
}

// mux 协议常量（客户端侧独立副本，与服务端常量互为对照）。
const (
	statusNew       = 0x01
	statusKeep      = 0x02
	statusEnd       = 0x03
	statusKeepAlive = 0x04
	optionData      = 0x01
	networkTCP      = 0x01
	addrTypeIPv4    = 0x01
	addrTypeDomain  = 0x02
)

// newFrame 按协议规范逐字节拼帧。
func newFrame(id uint16, status, option byte, host string, port int, data []byte) []byte {
	meta := []byte{byte(id >> 8), byte(id), status, option}
	if status == statusNew {
		meta = append(meta, networkTCP)
		meta = append(meta, byte(port>>8), byte(port))
		meta = append(meta, encodeAddr(host)...)
	}
	buf := make([]byte, 2, 2+len(meta)+2+len(data))
	binary.BigEndian.PutUint16(buf, uint16(len(meta)))
	buf = append(buf, meta...)
	if option&optionData != 0 {
		buf = append(buf, byte(len(data)>>8), byte(len(data)))
		buf = append(buf, data...)
	}
	return buf
}

// encodeAddr 按规范编码地址段（IPv4 字面量 / 域名）。
func encodeAddr(host string) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return append([]byte{addrTypeIPv4}, v4...)
		}
		return append([]byte{0x03}, ip.To16()...)
	}
	return append([]byte{addrTypeDomain, byte(len(host))}, host...)
}
