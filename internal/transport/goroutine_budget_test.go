package transport_test

// 一条用户请求到底拉起多少协程：在已建立的会话上发起单个 Forward 请求、保持在途，
// 对比请求前后的全量 goroutine 栈，按栈特征归桶。两端（服务端与客户端代理）在同一
// 测试进程里，桶里能看到两侧的协程；服务端侧的断言是本测试的重点：
//
//	请求驱动协程（tcp.stream→handleForward→ProxyBidirectional→wg.Wait）= 1
//	双向搬运泵（pump.up / pump.down）                                  = 2
//	yamux 会话循环（recv/send/keepalive）增量                          = 0（按会话，不按流）
//
// yamux 只在 OpenStream 侧有一个瞬时的 setOpenTimeout 守卫（流 ACK 后退出），
// 回环上基本抓不到，只记录不断言。

import (
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/core"
	"github.com/hhy50/vnray/internal/protocol"
)

// TestUserRequestGoroutineBudget 数清单条在途用户请求的协程开销。
func TestUserRequestGoroutineBudget(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "ok")
	})
	// 心跳关闭：避免保活拍点上的瞬时 pong 协程污染采样。
	h := newProxyHarness(t, directDispatcher(), handler, core.HeartbeatOptions{})

	// 等握手、accept 循环、TCP 层全部安静下来再取基线。
	time.Sleep(300 * time.Millisecond)
	baseNum := runtime.NumGoroutine()
	before := stackSnapshot()

	fwd, err := h.user.Forward(protocol.Address{Host: h.dest.Host, Port: uint16(h.dest.Port)})
	if err != nil {
		t.Fatalf("user forward: %v", err)
	}
	defer func() { _ = fwd.Close() }()
	request := "GET / HTTP/1.1\r\nHost: " + h.dest.String() + "\r\nConnection: close\r\n\r\n"
	if _, err := fwd.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("origin handler never entered")
	}
	// 隧道已建立、各方均已就位；再留一拍让"注定要退出的瞬时协程"退完。
	time.Sleep(200 * time.Millisecond)
	after := stackSnapshot()

	delta := stackDeltaOf(before, after)
	t.Logf("协程数: 基线=%d 在途=%d 净增=%d", baseNum, runtime.NumGoroutine(), delta.total())
	logGoroutineBuckets(t, delta)

	// --- 服务端侧断言 ---
	// 注意用 "ProxyBidirectional(" 精确匹配：泵协程的栈里有外层闭包
	// ProxyBidirectional.func1，裸匹配会把泵也算进来。
	if got := delta.count("transport.ProxyBidirectional("); got != 1 {
		t.Errorf("server request-driver goroutines (ProxyBidirectional→wg.Wait) = %d, want 1", got)
	}
	if got := delta.count("transport.(*pipeline).run"); got != 2 {
		t.Errorf("server pump goroutines (pipeline.run) = %d, want 2", got)
	}
	yamuxSessionLoops := 0
	for _, sig := range []string{"yamux.(*Session).recv(", "yamux.(*Session).send(", "yamux.(*Session).keepalive("} {
		// 同一条会话协程在 select 不同 case 重新停泊会产生不同栈文本，
		// 文本级 diff 会误报；这里对比前后快照的签名总数是否一致。
		if countIn(before, sig) != countIn(after, sig) {
			yamuxSessionLoops++
		}
	}
	if yamuxSessionLoops != 0 {
		t.Errorf("yamux per-session loop count changed across the request, want stable (loops are per session, not per stream)")
	}

	// --- 请求结束后协程回收 ---
	// 半关闭语义：响应搬完时服务端只对用户流发 FIN（读方向保留），pump.up 要等
	// 用户侧关流才退出——所以先由用户侧收口，再验证服务端协程全部回收。
	close(release)
	_ = fwd.Close()
	buf := make([]byte, 4<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		dump := string(buf[:n])
		if !strings.Contains(dump, "transport.(*pipeline).run") &&
			!strings.Contains(dump, "transport.ProxyBidirectional(") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 收尾失败：把还活着的请求路径协程栈（含客户端侧）打出来定位卡点。
	n := runtime.Stack(buf, true)
	for _, chunk := range splitGoroutines(string(buf[:n])) {
		if strings.Contains(chunk, "transport.ProxyBidirectional(") ||
			strings.Contains(chunk, "transport.(*pipeline).run") ||
			strings.Contains(chunk, "client.(*Client).handleTunnel") ||
			strings.Contains(chunk, "processTCP") ||
			strings.Contains(chunk, "dispatch.(*DefaultDispatcher)") {
			t.Logf("仍未退出:\n%s", chunk)
		}
	}
	t.Error("request goroutines did not wind down after completion")
}

// splitGoroutines 按 "goroutine N [...]" 头把全量 dump 切成单协程块。
// 不能按空行切：协程块之间的分隔不保证恰好是 "\n\n"。
func splitGoroutines(dump string) []string {
	lines := strings.Split(dump, "\n")
	var out []string
	var cur []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "goroutine ") && len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// stackSnapshot 抓全量栈，按"去掉 goroutine 头一行"的栈体聚合计数。
// 测试失败时先怀疑缓冲截断：t.Fatalf 保证 dump 完整，否则尾部协程会随机缺席，
// 把"基线里就有"的协程误报成净增。
func stackSnapshot() map[string]int {
	buf := make([]byte, 16<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		panic("stack dump truncated: buffer too small")
	}
	out := make(map[string]int)
	for _, chunk := range strings.Split(string(buf[:n]), "\n\n") {
		body := stripGoroutineHeader(chunk)
		if strings.TrimSpace(body) == "" {
			continue
		}
		out[normalizeStack(body)]++
	}
	return out
}

// normalizeStack 去掉栈里的 PC 偏移（+0x…）：同一条协程在 select 的不同 case
// 重新停泊时行内偏移会变，文本级 diff 会把它误报成"净增"。
func normalizeStack(body string) string {
	return stackOffsetRe.ReplaceAllString(body, "")
}

var stackOffsetRe = regexp.MustCompile(`\+0x[0-9a-f]+`)

// stripGoroutineHeader 去掉 "goroutine 123 [running]:" 头一行（含 id 与状态，不可比）。
func stripGoroutineHeader(chunk string) string {
	if i := strings.IndexByte(chunk, '\n'); i >= 0 {
		return chunk[i+1:]
	}
	return ""
}

// stackDeltaOf 是 after 相对 before 的净增栈（按出现次数差）。
// 采样协程自身的栈（含测试文件帧）两次行号不同、必然成对出现假差异，剔除。
func stackDeltaOf(before, after map[string]int) stackDelta {
	d := stackDelta{}
	for body, n := range after {
		if strings.Contains(body, "goroutine_budget_test.go") {
			continue
		}
		if extra := n - before[body]; extra > 0 {
			d[body] = extra
		}
	}
	return d
}

// countIn 统计快照里栈体含 match 的协程总数（按签名而非栈全文，规避重停泊偏移差异）。
func countIn(snap map[string]int, match string) int {
	total := 0
	for body, n := range snap {
		if strings.Contains(body, match) {
			total += n
		}
	}
	return total
}

// stackDelta 是净增栈集合：栈体 → 净增条数。
type stackDelta map[string]int

// total 返回净增协程总数。
func (d stackDelta) total() int {
	total := 0
	for _, n := range d {
		total += n
	}
	return total
}

// count 返回栈体里含 match 的净增协程数。
func (d stackDelta) count(match string) int {
	total := 0
	for body, n := range d {
		if strings.Contains(body, match) {
			total += n
		}
	}
	return total
}

// bucketOf 把一条栈归入可读的桶名；顺序敏感：先匹配更具体的特征。
func bucketOf(body string) string {
	switch {
	case strings.Contains(body, "transport.(*pipeline).run"):
		return "服务端: 双向搬运泵 pump.up/pump.down"
	case strings.Contains(body, "transport.ProxyBidirectional"):
		return "服务端: 请求驱动协程 (tcp.stream→handleForward→wg.Wait)"
	case strings.Contains(body, "yamux.(*Session).recv"),
		strings.Contains(body, "yamux.(*Session).send"),
		strings.Contains(body, "yamux.(*Session).keepalive"):
		return "yamux: 会话循环（按会话不按流）"
	case strings.Contains(body, "yamux.(*Session).setOpenTimeout"):
		return "yamux: 流打开超时守卫（瞬时）"
	case strings.Contains(body, "client.(*Client).handleTunnel.func"):
		return "客户端: watchClose/tunnel.up/tunnel.down"
	case strings.Contains(body, "client.(*Client).handleTunnel"):
		return "客户端: 请求驱动协程 (handleTunnel→wg.Wait)"
	case strings.Contains(body, "dispatch."):
		return "客户端: 出站分发与直连拨号"
	case strings.Contains(body, "userclient."):
		return "用户侧: 会话辅助协程"
	case strings.Contains(body, "http."),
		strings.Contains(body, "net."):
		return "源站/网络: net-http 与连接协程"
	default:
		return "其他"
	}
}

// logGoroutineBuckets 按桶打印净增协程与每桶一条示例栈（截断）。
func logGoroutineBuckets(t *testing.T, d stackDelta) {
	t.Helper()
	example := map[string]string{}
	total := 0
	for body, n := range d {
		total += n
		b := bucketOf(body)
		if _, ok := example[b]; !ok {
			s := body
			if len(s) > 1500 {
				s = s[:1500] + "…"
			}
			example[b] = s
		}
	}
	t.Logf("净增协程总数: %d", total)
	for b, s := range example {
		n := 0
		for body, cnt := range d {
			if bucketOf(body) == b {
				n += cnt
			}
		}
		t.Logf("[%s] ×%d\n示例栈:\n%s", b, n, s)
	}
}
