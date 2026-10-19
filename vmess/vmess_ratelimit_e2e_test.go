package vmess_test

// v2ray(vmess) 协议入站限速的端到端验证。
//
// 最终链路（与用户描述一致）：
//
//	A(vmess 模拟客户端) → C(项目服务端 v2ray 插件入站) → D(项目客户端隧道)
//	                     → B(HTTP 下载源站，与 D 同网、经 D 中转可达)
//
// 服务端 C 经 v2ray 插件把 uuid 用户路由到客户端 D，D 用直连出站访问 B；
// 限速挂在 uuid 身份上（registry 按 Identity(UserTypeUUID, uuid) 建桶），
// 由 ProxyBidirectional 的 DownLimit 约束 B→A 的下载方向。
//
// 跑四个场景：不限速（完整下完 128MiB 并校验 sha256）、10MB/s、1MB/s、100KB/s；
// 后三者用 4 秒采样窗口核对速率，避免 100KB/s 下完整下完要 20+ 分钟。

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

const rateLimitUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// TestV2RayPluginRateLimitDownload 验证 v2ray 入站限速：128MiB 下载跑四档速率。
func TestV2RayPluginRateLimitDownload(t *testing.T) {
	testV2RayRateLimitDownload(t, true)
}

// TestV2RayRateLimitOpenRequest 验证请求方向保持打开时的真实 VMess 下载限速。
func TestV2RayRateLimitOpenRequest(t *testing.T) {
	testV2RayRateLimitDownload(t, false)
}

func testV2RayRateLimitDownload(t *testing.T, finishRequest bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping 128MiB v2ray rate-limit scenarios in short mode")
	}

	// B：HTTP 下载源站，流式吐 128MiB 不可压缩的确定性数据；记下完整哈希供不限速场景校验。
	const fileSize = 128 << 20
	var (
		hashMu     sync.Mutex
		originHash string
	)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hasher := sha256.New()
		buf := make([]byte, 64<<10)
		var state uint64 = 0x9E3779B97F4A7C15
		for written := int64(0); written < fileSize; {
			for i := range buf {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				buf[i] = byte(state >> 24)
			}
			n, err := w.Write(buf)
			_, _ = hasher.Write(buf[:n])
			if err != nil {
				return // 窗口场景下载方到期断开，属预期
			}
			written += int64(n)
		}
		hashMu.Lock()
		originHash = hex.EncodeToString(hasher.Sum(nil))
		hashMu.Unlock()
	}))
	t.Cleanup(origin.Close)
	oHost, oPortStr, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("parse origin addr: %v", err)
	}
	originPort, _ := strconv.Atoi(oPortStr)

	// C：数据面服务（ConnectorService）+ v2ray 插件；限速注册表非 nil 并接进插件与观察者。
	cfg := &config.Server{
		Server: config.ServerOptions{
			Listen: "127.0.0.1:0",
			Auth:   config.Auth{Key: "rl-key"},
		},
	}
	sessions := transport.NewSessionManager()
	svc := core.NewConnectorService(cfg, sessions, nil, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start data plane: %v", err)
	}
	t.Cleanup(func() { _ = svc.Drain(context.Background()) })

	// D：项目客户端，与 B 同网（本测试里都在本机），用直连出站访问 B。
	agent, err := client.Dial(svc.Addr(), directDispatcher(), client.DialOptions{
		Auth: client.AuthOptions{Key: "rl-key"},
		Info: protocol.ClientInfo{Name: "test-edge", Version: "rl", OS: "test", Arch: "test"},
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	go func() { _ = agent.Serve() }()

	deadline := time.Now().Add(5 * time.Second)
	for svc.Sessions().ClientCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client session never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	st := testStore(t)
	bindings := admin.NewBindings(st)
	limits := ratelimit.NewRegistry()
	plugins := plugin.NewManager(st, bindings, svc.Sessions(), limits)
	// 与生产装配一致：用户表变更同时推给限速注册表（按 uuid 身份建桶）与 v2ray 验证器（路由）。
	bindings.Watch(func(c admin.UserChange) {
		if c.Deleted {
			limits.Forget(ratelimit.Identity(c.Protocol, c.Key))
		} else {
			limits.Apply(ratelimit.Identity(c.Protocol, c.Key), c.Config.UpLimit, c.Config.DownLimit)
		}
		if p, e := plugins.Get("v2ray"); e == nil {
			if syncer, ok := p.(plugin.UserSyncer); ok {
				syncer.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
			}
		}
	})
	if err := plugins.Start(context.Background()); err != nil {
		t.Fatalf("start plugins: %v", err)
	}
	v2ray, err := plugins.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}
	vPort, vlessPort := freePort(t), freePort(t)
	if err := v2ray.Enable(context.Background(), map[string]string{
		"vmess_port": strconv.Itoa(vPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable v2ray: %v", err)
	}

	// setLimit 更新该 uuid 用户的限速配置（同时推限速桶与路由）；Client 固定为 test-edge。
	setLimit := func(upBps, downBps int64) {
		if err := bindings.PutUser(store.UserBinding{
			Key:      rateLimitUUID,
			Protocol: store.ProtocolVMess,
			Name:     "e2e-ratelimit",
			Status:   store.UserEnabled,
			Client:   "test-edge",
			Config:   store.UserConfig{UpLimit: upBps, DownLimit: downBps},
		}); err != nil {
			t.Fatalf("put user: %v", err)
		}
	}
	// 先登记一次（不限速）让路由与验证器就绪；随后各场景仅变更速率。
	setLimit(0, 0)

	scenarios := []struct {
		name    string
		upBps   int64
		downBps int64
		full    bool // 是否下完整个文件并校验哈希
	}{
		{name: "unlimited", full: true},
		{name: "limited-10M", upBps: 10 << 20, downBps: 10 << 20},
		{name: "limited-1M", upBps: 1 << 20, downBps: 1 << 20},
		{name: "limited-100k", upBps: 100 << 10, downBps: 100 << 10},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			setLimit(sc.upBps, sc.downBps)
			// 校验限速确实被推入注册表（身份按 uuid 命名空间）。
			identity := ratelimit.Identity(store.ProtocolVMess, rateLimitUUID)
			if up, down := limits.Limits(identity); up != sc.upBps || down != sc.downBps {
				t.Fatalf("limits not applied: got (%d,%d) want (%d,%d)", up, down, sc.upBps, sc.downBps)
			}

			conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(vPort))
			if err != nil {
				t.Fatalf("dial v2ray: %v", err)
			}
			defer conn.Close()
			// A：vmess 模拟客户端连入 v2ray 入站，目标指到 B。
			sess, err := vmess.NewClientSession(rateLimitUUID, conn,
				vmess.Target{Host: oHost, Port: originPort},
				vmess.SecurityAES128GCM, vmess.OptionChunkStream|vmess.OptionChunkMasking)
			if err != nil {
				t.Fatalf("vmess handshake: %v", err)
			}
			if err := sess.ReadResponseHeader(); err != nil {
				t.Fatalf("read response header: %v", err)
			}
			w := sess.BodyWriter()
			request := "GET /big.bin HTTP/1.1\r\nHost: " + oHost + "\r\nConnection: close\r\n\r\n"
			if _, err := w.Write([]byte(request)); err != nil {
				t.Fatalf("write request: %v", err)
			}
			// 保留终结帧路径的原有回归，不用限速测试掩盖方向关闭缺陷。
			if finishRequest {
				_ = w.Close()
			}

			if sc.full {
				resp, err := http.ReadResponse(bufio.NewReader(sess.BodyReader()), nil)
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
				if n != fileSize {
					t.Fatalf("downloaded %d bytes, want %d", n, fileSize)
				}
				hashMu.Lock()
				want := originHash
				hashMu.Unlock()
				if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
					t.Fatalf("sha256 mismatch:\n got %s\nwant %s", got, want)
				}
				t.Logf("v2ray 不限速：%.0f MiB 完整下载并通过 sha256 校验", float64(n)/(1<<20))
				return
			}

			// 限速场景：底层 conn 设 4s 读超时做采样窗口，核对收到字节数落在容差带内。
			const window = 4 * time.Second
			_ = conn.SetReadDeadline(time.Now().Add(window))
			nominal := sc.downBps * int64(window/time.Second)
			start := time.Now()
			var received int64
			buf := make([]byte, 32<<10)
			for {
				n, rerr := sess.BodyReader().Read(buf)
				received += int64(n)
				if rerr != nil {
					break // 窗口到期（超时）或对端结束
				}
			}
			elapsed := time.Since(start)
			// 令牌桶容量为 1 秒速率（至多突发 1 秒的量），故 window 秒窗口内理论上限为
			// 速率×(window+1)；上界据此放宽并留少量余量。下界取名义值的 60%。
			min, max := nominal*6/10, nominal+sc.downBps+64<<10
			if received < min || received > max {
				t.Fatalf("限速 %d KB/s 下 %s 收到 %d 字节（%.0f KB/s），超出 [%d, %d]",
					sc.downBps/1024, elapsed.Round(time.Millisecond), received,
					float64(received)/(1<<10)/elapsed.Seconds(), min, max)
			}
			t.Logf("v2ray 限速 %d KB/s：%.1fs 收到 %.0f KB（实测 %.0f KB/s，名义 %d KB/s）",
				sc.downBps/1024, elapsed.Seconds(), float64(received)/(1<<10),
				float64(received)/(1<<10)/elapsed.Seconds(), sc.downBps/1024)
		})
	}
}
