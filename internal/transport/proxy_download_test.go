package transport_test

// 端到端的数据面集成测试：真实起 HTTP 源站、数据面服务（ConnectorService）与
// 客户端代理（Client），再由服务端侧会话发起 HTTP 请求，验证字节确实经客户端代理取回。
//
// 链路：
//
//	HTTP 源站 ← dispatch.DirectOutbound ← dispatch.DefaultDispatcher ← Client
//	          ← 多路复用会话（由 tcp.Server 建立）← core.ConnectorService ← transport.Session.OpenStream（本测试驱动）
//
// 会话句柄通过 SessionEvents 拿到——生产环境里这活由 admin.Sessions 干，这里换成
// 测试专用的收集器，好在服务端侧直接开流。用导出的 transport.Session.OpenStream 而非
// transport.Session.Request，是为了让响应以常量内存流过：后者会把整个响应缓冲成 string，
// 128 MiB 的响应会白白多占几百 MB。
//
// 想快速跳过重测试：go test -short ./internal/transport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/acl"
	"github.com/hhy50/vnray/internal/client"
	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/dispatch"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/internal/userclient"
)

// downloadFileSize 是下载测试的文件大小：128 MiB，明确大于 100 MB。
// 这会真写 128 MiB 到 t.TempDir() 并跑满一次回环传输，所以 -short 下跳过。
const downloadFileSize = 128 << 20

// testAuthKey 是集成测试两端共用的认证密钥；两端一致才建会话。
const testAuthKey = "test-shared-key"

// testClientInfo 是测试客户端随认证上报的配置与环境，供往返断言。
func testClientInfo() protocol.ClientInfo {
	return protocol.ClientInfo{
		Version:  "v-test",
		OS:       "testos",
		Arch:     "testarch",
		Hostname: "test-host",
		PID:      4242,
		Server:   "127.0.0.1:9000",
		Public:   "10.0.0.0/8",
		Private:  "127.0.0.0/8",
		Log: &protocol.ClientLog{
			Level: "info", Output: "stderr",
		},
		Reconnect: &protocol.ClientReconnect{
			Enabled: true, Interval: "5s",
		},
		Routing: &protocol.ClientRouting{
			Default: "direct",
			Outbounds: []protocol.ClientOutbound{
				{Tag: "direct", Type: "direct"},
				{Tag: "reject", Type: "reject", Reason: "domain blocked by client rule"},
			},
			Rules: []protocol.ClientRule{
				{Name: "private-ip", Outbound: "direct", Match: protocol.ClientMatch{Host: []string{"127.0.0.1"}}},
			},
		},
	}
}

const (
	// downloadFileName 是源站上大文件的路径。
	downloadFileName = "big.bin"
	// sessionTimeout 等待客户端接入的上限。
	sessionTimeout = 10 * time.Second
	// transferTimeout 是单条请求流的读写上限，用于给挂死的测试兜底。
	transferTimeout = 3 * time.Minute
)

// planeOptions 是测试组装数据面服务端所需的参数：认证密钥、心跳策略、多路复用
// 配置与后端。sessionConfig/muxer 为 nil 时用默认值，两端必须一致。
type planeOptions struct {
	authKey       string
	heartbeat     core.HeartbeatOptions
	sessionConfig *tcp.SessionConfig
	muxer         tcp.Muxer
	limits        *ratelimit.Registry // 用户限速注册表；nil = 不限速
}

// testServerConfig 组装一份测试用服务端配置：监听端口交给内核分配。心跳不在配置里，
// 由 startPlane 按需经 ApplyHeartbeat 设置。
func testServerConfig(authKey string) *config.Server {
	return &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: authKey},
		},
	}
}

// startPlane 起一个测试用数据面服务并登记收尾，返回其句柄。
func startPlane(t *testing.T, popts planeOptions, registry transport.UserRegistry) *core.ConnectorService {
	t.Helper()

	mgr := transport.NewSessionManager()
	plane := core.NewConnectorServiceWith(
		testServerConfig(popts.authKey), mgr, registry, popts.limits,
		tcp.WithSessionConfig(popts.sessionConfig),
		tcp.WithMuxer(popts.muxer),
	)
	// 心跳不再来自配置：零值表示关闭，非零按给定周期探测。
	plane.ApplyHeartbeat(popts.heartbeat.Interval, popts.heartbeat.Timeout)
	if err := plane.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := plane.Drain(ctx); err != nil {
			t.Errorf("drain data plane: %v", err)
		}
	})
	return plane
}

// waitClientSession 轮询会话管理器，直到出现第一条客户端会话（替代旧的事件收集器）。
func waitClientSession(t *testing.T, mgr *transport.SessionManager) *transport.Session {
	t.Helper()
	deadline := time.Now().Add(sessionTimeout)
	for time.Now().Before(deadline) {
		if clients := mgr.Clients(); len(clients) > 0 {
			return clients[0].Client
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for client session")
	return nil
}

// proxyHarness 是测试用的最小数据面：HTTP 源站 + 数据面服务 + 客户端代理。
// 关闭顺序由 t.Cleanup 的逆序保证：客户端 → 服务端 → 源站。
type proxyHarness struct {
	origin   *httptest.Server
	dest     dispatch.Destination
	addr     string
	session  *transport.Session
	sessions *transport.SessionManager
	user     *userclient.Client

	mu      sync.Mutex
	hits    int
	reqLine string
}

// newProxyHarness 用默认认证密钥与默认多路复用配置组装数据面；hb 是服务端心跳策略（零值表示关闭）。
func newProxyHarness(t *testing.T, dispatcher dispatch.Dispatcher, handler http.Handler, hb core.HeartbeatOptions) *proxyHarness {
	t.Helper()
	return newProxyHarnessWith(t, dispatcher, handler, planeOptions{authKey: testAuthKey, heartbeat: hb},
		client.DialOptions{Auth: client.AuthOptions{Key: testAuthKey}, Info: testClientInfo()})
}

// newProxyHarnessWith 同 newProxyHarness，但由调用方指定两端选项（心跳、认证、多路复用配置）。
func newProxyHarnessWith(t *testing.T, dispatcher dispatch.Dispatcher, handler http.Handler, popts planeOptions, dopts client.DialOptions) *proxyHarness {
	t.Helper()
	h := &proxyHarness{}
	dopts.Info.Name = "c1" // 用户绑定按客户端名选路，客户端必须上报同名
	dopts.Muxer = popts.muxer

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits++
		h.reqLine = r.Method + " " + r.URL.Path + " host=" + r.Host
		h.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	h.origin = origin
	t.Cleanup(origin.Close)

	dest, err := dispatch.ParseDestination(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin address: %v", err)
	}
	h.dest = dest

	// 端口交给内核分配：服务在 Start 之后回读真实端口，
	// 不必用"先占端口再释放"那种带竞争窗口的写法。
	plane := startPlane(t, popts, stubUserRegistry{limitedUserKey: "c1"})
	h.sessions = plane.Sessions()
	h.addr = plane.Addr()

	client, err := client.Dial(plane.Addr(), dispatcher, dopts)
	if err != nil {
		t.Fatalf("dial data plane: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- client.Serve() }()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("client serve: %v", err)
			}
		case <-time.After(sessionTimeout):
			t.Error("client serve did not return after close")
		}
	})

	h.session = waitClientSession(t, h.sessions)
	// 客户端会话就绪后再接用户：Forward 选路依赖按名索引。
	u, err := userclient.Dial(h.addr, protocol.UserAuthRequest{Key: limitedUserKey}, popts.sessionConfig, popts.muxer)
	if err != nil {
		t.Fatalf("dial user: %v", err)
	}
	h.user = u
	return h
}

// originRequests 返回源站被访问的次数与最后一条请求行。
func (h *proxyHarness) originRequests() (int, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits, h.reqLine
}

// proxiedRequest 是一次经代理请求的结果：HTTP 响应 + 底层隧道流（由调用方关闭）。
type proxiedRequest struct {
	*http.Response
	closer io.Closer
}

func (p *proxiedRequest) close() { _ = p.closer.Close() }

// fetchThroughProxy 通过用户 Forward 隧道发一个 HTTP GET（Connection: close）并返回
// 响应头；正文由调用方从 resp.Body 读。代理以 ForwardResult 拒绝时返回该原因作为 error。
func (h *proxyHarness) fetchThroughProxy(t *testing.T, dest dispatch.Destination, path string) (*proxiedRequest, error) {
	t.Helper()
	fwd, err := h.user.Forward(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
	if err != nil {
		return nil, err
	}
	if err := fwd.SetDeadline(time.Now().Add(transferTimeout)); err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("set stream deadline: %w", err)
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: dispatcher-demo-test\r\nConnection: close\r\n\r\n",
		path, dest.String())
	if _, err := fwd.Write([]byte(request)); err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReaderSize(fwd, 64<<10), nil)
	if err != nil {
		_ = fwd.Close()
		return nil, err
	}
	return &proxiedRequest{Response: resp, closer: fwd}, nil
}

// stubUserRegistry 是最小 UserRegistry：静态 user_key → 客户端 绑定（一律视为启用）。
type stubUserRegistry map[string]string

func (r stubUserRegistry) UserFor(key string) (transport.UserAccess, bool) {
	c, ok := r[key]
	return transport.UserAccess{Client: c, Name: key, Active: true}, ok
}

// limitedUserKey 是 harness 里用户出示的密钥；注册表把它绑定到客户端 c1。
const limitedUserKey = "limited-user-key"

// directDispatcher 返回最朴素的直连分发器（无 ACL）。
func directDispatcher() dispatch.Dispatcher {
	return dispatch.NewDefaultDispatcher(nil, "direct", dispatch.NewDirectOutbound("direct"))
}

// TestClientProxyHeartbeat 验证服务端主动心跳：Ping 带毫秒时间戳、客户端原样回显
// Pong，会话在若干拍内即可读到测得 RTT 与最近心跳时刻，且不影响同会话的请求路径。
//
// 用很短的间隔（20ms）把"等第一拍"压到百毫秒级；断言只要求数值落在合理区间，
// 不锁定具体时延，免得在负载波动的 CI 上抖。
func TestClientProxyHeartbeat(t *testing.T) {
	const interval = 20 * time.Millisecond

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	h := newProxyHarness(t, directDispatcher(), handler,
		core.HeartbeatOptions{Interval: interval, Timeout: time.Second})

	// 轮询等第一拍落地。等 LastHeartbeat 而不是 LastRTT：心跳循环先写 RTT 再写时刻，
	// 观察到时刻非零即说明两个字段都已就绪，不会读到半个状态。
	deadline := time.Now().Add(5 * time.Second)
	for h.session.LastHeartbeat().IsZero() && time.Now().Before(deadline) {
		time.Sleep(interval / 4)
	}
	if h.session.LastHeartbeat().IsZero() {
		t.Fatalf("no heartbeat measured within %s", 5*time.Second)
	}

	rtt := h.session.LastRTT()
	if rtt <= 0 {
		t.Fatalf("rtt = %s, want > 0", rtt)
	}
	if rtt >= time.Second {
		t.Fatalf("rtt %s is implausibly large for loopback", rtt)
	}
	if ago := time.Since(h.session.LastHeartbeat()); ago < 0 || ago > 5*time.Second {
		t.Fatalf("last heartbeat %s ago is out of range", ago)
	}

	// 心跳不碰 dispatcher/ACL，正常的请求路径必须不受影响。
	resp, err := h.fetchThroughProxy(t, h.dest, "/ping")
	if err != nil {
		t.Fatalf("request after heartbeat: %v", err)
	}
	defer resp.close()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status after heartbeat = %s, want 200 OK", resp.Status)
	}
	if hits, _ := h.originRequests(); hits != 1 {
		t.Errorf("origin received %d requests, want 1", hits)
	}

	// 首拍之后仍在持续刷新。
	first := h.session.LastHeartbeat()
	deadline = time.Now().Add(5 * time.Second)
	for !h.session.LastHeartbeat().After(first) && time.Now().Before(deadline) {
		time.Sleep(interval / 4)
	}
	if !h.session.LastHeartbeat().After(first) {
		t.Error("heartbeat stopped refreshing after the first beat")
	}
}

// TestUserForwardDownload 按最终链路 A→C→D→B 下载 128 MiB 大文件，四个场景：
// 不限速（完整下载 + sha256 完整性）、10MB/s、1MB/s、100KB/s——后三者用采样窗口
// 验证限速精度（下完 100KB/s 要 22 分钟，所以只取 4 秒窗口计数）。
func TestUserForwardDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 128MiB download scenarios in short mode")
	}

	// 源站 B：流式吐 downloadFileSize 字节的确定性 pattern（xorshift，不可压缩），
	// 并记下完整写出的哈希——不限速场景会下完整个文件做比对。
	var (
		hashMu     sync.Mutex
		originHash string
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hasher := sha256.New()
		buf := make([]byte, 64<<10)
		var state uint64 = 0x9E3779B97F4A7C15
		for written := int64(0); written < downloadFileSize; {
			for i := range buf {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				buf[i] = byte(state >> 24)
			}
			n, err := w.Write(buf)
			_, _ = hasher.Write(buf[:n])
			if err != nil {
				return // 窗口场景的下载方到期断开，属预期
			}
			written += int64(n)
		}
		hashMu.Lock()
		originHash = hex.EncodeToString(hasher.Sum(nil))
		hashMu.Unlock()
	})

	limits := ratelimit.NewRegistry()
	// 绑定表的键是裸 user_key；限速注册表的身份键由 ratelimit.Identity 从协议派生，
	// 这里同样用 Identity 构造，避免两边的前缀各写一遍而漂移。
	registry := stubUserRegistry{
		"unlim": "c1",
		"10m":   "c1",
		"1m":    "c1",
		"100k":  "c1",
	}
	plane := startPlane(t, planeOptions{authKey: testAuthKey, limits: limits}, registry)
	mgr := plane.Sessions()

	origin := httptest.NewServer(handler)
	t.Cleanup(origin.Close)
	dest, err := dispatch.ParseDestination(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin address: %v", err)
	}

	client, err := client.Dial(plane.Addr(), directDispatcher(),
		client.DialOptions{Auth: client.AuthOptions{Key: testAuthKey},
			Info: func() protocol.ClientInfo {
				info := testClientInfo()
				info.Name = "c1" // 用户绑定按客户端名选路，客户端必须上报同名
				return info
			}()})
	if err != nil {
		t.Fatalf("dial data plane: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- client.Serve() }()
	t.Cleanup(func() { _ = client.Close() })
	waitClientSession(t, mgr)

	scenarios := []struct {
		name   string
		key    string
		upKB   int64 // KB/s，0 = 不限
		downKB int64
		full   bool // 是否下完整个文件并校验哈希
	}{
		{name: "unlimited", key: "unlim", full: true},
		{name: "limited-10MB-s", key: "10m", upKB: 10 << 10, downKB: 10 << 10},
		{name: "limited-1MB-s", key: "1m", upKB: 1 << 10, downKB: 1 << 10},
		{name: "limited-100KB-s", key: "100k", upKB: 100, downKB: 100},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			limits.Apply(ratelimit.Identity(store.ProtocolVNV1, sc.key), sc.upKB*1024, sc.downKB*1024)
			u, err := userclient.Dial(plane.Addr(),
				protocol.UserAuthRequest{Key: sc.key}, nil, nil)
			if err != nil {
				t.Fatalf("dial user: %v", err)
			}
			t.Cleanup(func() { _ = u.Close() })
			fwd, err := u.Forward(protocol.Address{Host: dest.Host, Port: uint16(dest.Port)})
			if err != nil {
				t.Fatalf("forward: %v", err)
			}
			defer func() { _ = fwd.Close() }()
			if err := fwd.SetDeadline(time.Now().Add(transferTimeout)); err != nil {
				t.Fatal(err)
			}
			request := fmt.Sprintf("GET /%s HTTP/1.1\r\nHost: proxy-test\r\nConnection: close\r\n\r\n", downloadFileName)
			if _, err := fwd.Write([]byte(request)); err != nil {
				t.Fatalf("write request: %v", err)
			}

			if sc.full {
				start := time.Now()
				resp, err := http.ReadResponse(bufio.NewReaderSize(fwd, 64<<10), nil)
				if err != nil {
					t.Fatalf("read response: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status through proxy = %s, want 200 OK", resp.Status)
				}
				hasher := sha256.New()
				n, err := io.Copy(hasher, resp.Body)
				if err != nil {
					t.Fatalf("stream body: %v", err)
				}
				elapsed := time.Since(start)
				if n != int64(downloadFileSize) {
					t.Fatalf("downloaded %d bytes, want %d", n, downloadFileSize)
				}
				hashMu.Lock()
				want := originHash
				hashMu.Unlock()
				if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
					t.Fatalf("sha256 mismatch:\n got %s\nwant %s", got, want)
				}
				mb := float64(n) / (1 << 20)
				t.Logf("不限速：%.0f MiB 用时 %s（%.1f MiB/s）", mb, elapsed.Round(time.Millisecond), mb/elapsed.Seconds())
				return
			}

			// 限速场景：采样 4 秒窗口。名义值 = 速率 × 窗口（首桶突发恰好覆盖第 1 秒）。
			const window = 4 * time.Second
			if err := fwd.SetDeadline(time.Now().Add(window)); err != nil {
				t.Fatal(err)
			}
			nominal := sc.downKB * 1024 * int64(window/time.Second)
			start := time.Now()
			var received int64
			buf := make([]byte, 32<<10)
			for {
				n, err := fwd.Read(buf)
				received += int64(n)
				if err != nil {
					break // 窗口到期（timeout）或对端结束
				}
			}
			elapsed := time.Since(start)
			min, max := nominal*6/10, nominal*115/100+64<<10
			if received < min || received > max {
				t.Fatalf("限速 %dKB/s 下 %s 收到 %d 字节（%.0f KB/s），超出 [%d, %d]",
					sc.downKB, elapsed.Round(time.Millisecond), received,
					float64(received)/(1<<10)/elapsed.Seconds(), min, max)
			}
			t.Logf("限速 %dKB/s：%.1fs 收到 %.0f KB（实测 %.0f KB/s，名义 %d KB/s）",
				sc.downKB, elapsed.Seconds(), float64(received)/(1<<10),
				float64(received)/(1<<10)/elapsed.Seconds(), sc.downKB)
		})
	}
}

// TestUserForwardACL 验证 acl.Guard 在 D 侧 dispatcher 上对用户 Forward 生效：
// 拒绝时 ForwardResult 带原因、源站零访问；放行时正文原样到达。
func TestUserForwardACL(t *testing.T) {
	const body = "hello through the acl guard"

	cases := []struct {
		name       string
		public     string
		private    string
		wantDenied bool
	}{
		{name: "allowed by public allowlist", public: "127.0.0.0/8"},
		{name: "denied by private blocklist", private: "127.0.0.0/8", wantDenied: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard, err := acl.NewGuard(tc.public, tc.private)
			if err != nil {
				t.Fatalf("build acl guard: %v", err)
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			h := newProxyHarness(t, guard.Wrap(directDispatcher()), handler, core.HeartbeatOptions{})

			u, uerr := userclient.Dial(h.addr, protocol.UserAuthRequest{Key: limitedUserKey}, nil, nil)
			if uerr != nil {
				t.Fatalf("dial user: %v", uerr)
			}
			t.Cleanup(func() { _ = u.Close() })
			fwd, ferr := u.Forward(protocol.Address{Host: h.dest.Host, Port: uint16(h.dest.Port)})

			if tc.wantDenied {
				if ferr == nil {
					_ = fwd.Close()
					t.Fatal("forward should have been denied by acl")
				}
				for _, want := range []string{"acl:", "denied"} {
					if !strings.Contains(ferr.Error(), want) {
						t.Fatalf("acl denial not surfaced through proxy: %v", ferr)
					}
				}
				if hits, line := h.originRequests(); hits != 0 {
					t.Errorf("origin was contacted %d time(s) despite acl denial: %q", hits, line)
				}
				return
			}
			if ferr != nil {
				t.Fatalf("forward through proxy: %v", ferr)
			}
			defer func() { _ = fwd.Close() }()

			request := "GET /guarded HTTP/1.1\r\nHost: proxy-test\r\nConnection: close\r\n\r\n"
			if _, err := fwd.Write([]byte(request)); err != nil {
				t.Fatalf("write request: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReaderSize(fwd, 64<<10), nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status through proxy = %s, want 200 OK", resp.Status)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body through proxy: %v", err)
			}
			if string(got) != body {
				t.Fatalf("body through proxy = %q, want %q", got, body)
			}
			if hits, _ := h.originRequests(); hits != 1 {
				t.Errorf("origin received %d requests, want 1", hits)
			}
		})
	}
}

// TestAuthHandshake 验证握手认证：密钥一致才建会话，且服务端能读到客户端上报的配置与环境；
// 密钥不一致（或客户端未上报）时拨号被拒（client.ErrAuthRejected），服务端始终不登记会话。
func TestAuthHandshake(t *testing.T) {
	cases := []struct {
		name      string
		serverKey string
		clientKey string
		wantOK    bool
	}{
		{name: "key matches", serverKey: "shared-key-1", clientKey: "shared-key-1", wantOK: true},
		{name: "key mismatch", serverKey: "shared-key-1", clientKey: "shared-key-2"},
		{name: "client sends no key", serverKey: "shared-key-1"},
		{name: "server has no key", serverKey: "", clientKey: "anything", wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane := startPlane(t, planeOptions{authKey: tc.serverKey}, nil)

			agent, err := client.Dial(plane.Addr(), directDispatcher(), client.DialOptions{
				Auth: client.AuthOptions{Key: tc.clientKey},
				Info: testClientInfo(),
			})

			if !tc.wantOK {
				if err == nil {
					_ = agent.Close()
					t.Fatal("dial should have been rejected")
				}
				if !errors.Is(err, client.ErrAuthRejected) {
					t.Fatalf("dial error = %v, want client.ErrAuthRejected", err)
				}
				// 收尾后再断言：认证被拒时服务端自始至终没有登记过会话。
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if derr := plane.Drain(ctx); derr != nil {
					t.Errorf("drain data plane: %v", derr)
				}
				if n := plane.Sessions().ClientCount(); n != 0 {
					t.Fatalf("server registered %d session(s) despite rejected auth", n)
				}
				return
			}

			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = agent.Close() }()

			// testClientInfo 返回 client 项目的 ClientInfo 副本；服务端登记的是
			// 本包类型，字段逐项一致即可（跨包类型不能直接 DeepEqual，先转换）。
			want := protocol.ClientInfo(testClientInfo())
			if got := waitClientSession(t, plane.Sessions()).Info(); !reflect.DeepEqual(got, want) {
				t.Fatalf("reported client info = %+v, want %+v", got, want)
			}
		})
	}
}

// 长连接测试用参数：keepalive 周期压到很小，好让"跨越多个 keepalive 超时窗口"在秒级完成。
const (
	longConnHeartbeatInterval = 100 * time.Millisecond
	longConnRequestInterval   = 500 * time.Millisecond
	longConnKeepAliveInterval = 200 * time.Millisecond
	longConnKeepAliveTimeout  = time.Second
)

// longConnSessionConfig 返回压缩后的多路复用配置，供长连接测试压缩等待。
func longConnSessionConfig() *tcp.SessionConfig {
	cfg := tcp.DefaultSessionConfig()
	cfg.KeepAliveInterval = longConnKeepAliveInterval
	cfg.KeepAliveTimeout = longConnKeepAliveTimeout
	return cfg
}

// TestClientProxyLongLivedConnection 验证长连接在持续数倍 keepalive 超时窗口的时间里保持可用。
func TestClientProxyLongLivedConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping long-lived connection test in short mode")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	cfg := longConnSessionConfig()

	t.Run("持续请求", func(t *testing.T) {
		const duration = 6 * time.Second
		h := newProxyHarnessWith(t, directDispatcher(), handler,
			planeOptions{
				authKey:       testAuthKey,
				heartbeat:     core.HeartbeatOptions{Interval: longConnHeartbeatInterval, Timeout: time.Second},
				sessionConfig: cfg,
			},
			client.DialOptions{Auth: client.AuthOptions{Key: testAuthKey}, Info: testClientInfo(), SessionConfig: cfg})

		start := time.Now()
		firstHeartbeat := h.session.LastHeartbeat()
		var requests int
		for time.Since(start) < duration {
			time.Sleep(longConnRequestInterval)

			resp, err := h.fetchThroughProxy(t, h.dest, "/long-lived")
			if err != nil {
				t.Fatalf("request after %s: %v", time.Since(start).Round(time.Millisecond), err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.close()
			if err != nil || string(body) != "ok" {
				t.Fatalf("body after %s = %q, err = %v, want %q", time.Since(start).Round(time.Millisecond), body, err, "ok")
			}
			requests++
		}

		if hits, _ := h.originRequests(); hits != requests {
			t.Errorf("origin received %d requests, want %d", hits, requests)
		}
		if h.session.IsClosed() {
			t.Error("session was closed during the long-lived window")
		}
		if n := h.sessions.ClientCount(); n != 1 {
			t.Errorf("session count = %d, want 1 (the original session must stay registered throughout)", n)
		}
		last := h.session.LastHeartbeat()
		if !last.After(firstHeartbeat) {
			t.Errorf("heartbeat did not advance during the window: %s -> %s", firstHeartbeat, last)
		}
		if ago := time.Since(last); ago > 10*longConnHeartbeatInterval {
			t.Errorf("last heartbeat was %s ago, want it fresh", ago)
		}
		t.Logf("held one session for %s across %d keepalive timeout windows with %d successful requests",
			duration, int(duration/longConnKeepAliveTimeout), requests)
	})

	t.Run("空闲且关闭心跳", func(t *testing.T) {
		const duration = 3 * time.Second
		h := newProxyHarnessWith(t, directDispatcher(), handler,
			planeOptions{authKey: testAuthKey, sessionConfig: cfg},
			client.DialOptions{Auth: client.AuthOptions{Key: testAuthKey}, Info: testClientInfo(), SessionConfig: cfg})

		// 全程不发请求：只能靠多路复用 keepalive 的 Ping 交换保活。
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			if h.session.IsClosed() {
				t.Fatalf("idle session was closed after %s; mux keepalive should have kept it alive", duration)
			}
			if n := h.sessions.ClientCount(); n != 1 {
				t.Fatalf("session count = %d, want 1", n)
			}
			time.Sleep(50 * time.Millisecond)
		}

		// 空闲这么久之后，请求仍要能建立并成功。
		resp, err := h.fetchThroughProxy(t, h.dest, "/after-idle")
		if err != nil {
			t.Fatalf("request after idle: %v", err)
		}
		defer resp.close()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status after idle = %s, want 200 OK", resp.Status)
		}
		t.Logf("idle for %s across %d keepalive timeout windows and still usable",
			duration, int(duration/longConnKeepAliveTimeout))
	})
}
