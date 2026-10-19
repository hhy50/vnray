package mux

// mux 协议测试。客户端按协议规范逐字节构造帧（与生产编解码互为独立验证），
// 覆盖帧 literal、New/Keep/End/KeepAlive 全流程、并发子连接与异常路径。

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testTimeout 是所有等待步长的上限：net.Pipe 是同步通道，正常路径毫秒级完成。
const testTimeout = 3 * time.Second

// TestFrameLiterals 用规范字节 literal 驱动 readFrame，并用 literal 期望值
// 校验 writeFrame：编解码两端都直接对照协议规范，不互相抄。
func TestFrameLiterals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  []byte
		want Frame
	}{
		{
			name: "new ipv4 with first payload",
			raw: []byte{
				0x00, 0x0c, // metaLen = 2+1+1+1+2+1+4
				0x00, 0x07, // session id
				StatusNew, OptionData,
				NetworkTCP,
				0x23, 0x28, // port 9000
				addrTypeIPv4, 127, 0, 0, 1,
				0x00, 0x03, 'g', 'e', 't',
			},
			want: Frame{SessionID: 7, Status: StatusNew, Option: OptionData, Network: NetworkTCP,
				Target: Target{Host: "127.0.0.1", Port: 9000}, Data: []byte("get")},
		},
		{
			name: "new domain without payload",
			raw: []byte{
				0x00, 0x14, // metaLen = 2+1+1+1+2+1+1+11
				0x12, 0x34,
				StatusNew, 0,
				NetworkTCP,
				0x01, 0xbb, // port 443
				addrTypeDomain, 11, 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			},
			want: Frame{SessionID: 0x1234, Status: StatusNew, Network: NetworkTCP,
				Target: Target{Host: "example.com", Port: 443}},
		},
		{
			name: "new ipv6 keep-together",
			raw: []byte{
				0x00, 0x18, // metaLen = 2+1+1+1+2+1+16
				0xff, 0xff,
				StatusNew, OptionData,
				NetworkTCP,
				0x00, 0x50,
				addrTypeIPv6, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
				0x00, 0x00, // 空数据段：OptionData 置位但长度为零
			},
			want: Frame{SessionID: 0xffff, Status: StatusNew, Option: OptionData, Network: NetworkTCP,
				Target: Target{Host: "2001:db8::1", Port: 80}, Data: []byte{}},
		},
		{
			name: "keep with data",
			raw: []byte{
				0x00, 0x04,
				0x00, 0x09,
				StatusKeep, OptionData,
				0x00, 0x02, 'o', 'k',
			},
			want: Frame{SessionID: 9, Status: StatusKeep, Option: OptionData, Data: []byte("ok")},
		},
		{
			name: "keep without data",
			raw:  []byte{0x00, 0x04, 0x00, 0x09, StatusKeep, 0},
			want: Frame{SessionID: 9, Status: StatusKeep},
		},
		{
			name: "end",
			raw:  []byte{0x00, 0x04, 0x00, 0x09, StatusEnd, 0},
			want: Frame{SessionID: 9, Status: StatusEnd},
		},
		{
			name: "end with error",
			raw:  []byte{0x00, 0x04, 0x00, 0x09, StatusEnd, OptionError},
			want: Frame{SessionID: 9, Status: StatusEnd, Option: OptionError},
		},
		{
			name: "end with tail data",
			raw: []byte{
				0x00, 0x04,
				0x00, 0x09,
				StatusEnd, OptionData,
				0x00, 0x02, 'b', 'y',
			},
			want: Frame{SessionID: 9, Status: StatusEnd, Option: OptionData, Data: []byte("by")},
		},
		{
			name: "keepalive with data to discard",
			raw: []byte{
				0x00, 0x04,
				0xde, 0xad,
				StatusKeepAlive, OptionData,
				0x00, 0x01, 0x00,
			},
			want: Frame{SessionID: 0xdead, Status: StatusKeepAlive, Option: OptionData, Data: []byte{0}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFrame(newReader(tc.raw))
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if got.SessionID != tc.want.SessionID || got.Status != tc.want.Status ||
				got.Option != tc.want.Option || got.Network != tc.want.Network ||
				got.Target != tc.want.Target || string(got.Data) != string(tc.want.Data) {
				t.Fatalf("readFrame = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestFrameLiteralErrors 覆盖畸形帧的拒绝路径。
func TestFrameLiteralErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  []byte
	}{
		{"meta length over limit", []byte{0x02, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"meta truncated", []byte{0x00, 0x04, 0x00}},
		{"meta too short", []byte{0x00, 0x03, 0x00, 0x09, StatusKeep, 0}},
		{"new meta too short", []byte{0x00, 0x06, 0x00, 0x09, StatusNew, OptionData, NetworkTCP, 0x00}},
		{"data length truncated", []byte{0x00, 0x04, 0x00, 0x09, StatusKeep, OptionData, 0x00}},
		{"data truncated", []byte{0x00, 0x04, 0x00, 0x09, StatusKeep, OptionData, 0x00, 0x02, 'o'}},
		{"bad address type", []byte{0x00, 0x08, 0x00, 0x09, StatusNew, 0, NetworkTCP, 0x00, 0x50, 0x09, 127, 0, 0, 1}},
		{"trailing bytes in new meta", []byte{0x00, 0x0d, 0x00, 0x09, StatusNew, 0, NetworkTCP, 0x00, 0x50, addrTypeIPv4, 127, 0, 0, 1, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readFrame(newReader(tc.raw)); err == nil {
				t.Fatal("readFrame should fail on malformed frame")
			}
		})
	}
}

// TestWriteFrameLayout 校验写出的字节布局：与规范 literal 逐一对照。
func TestWriteFrameLayout(t *testing.T) {
	t.Parallel()
	var buf []byte
	w := &sliceWriter{&buf}
	if err := writeFrame(w, 0x1234, StatusKeep, OptionData, []byte("hi")); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	wantKeep := []byte{0x00, 0x04, 0x12, 0x34, StatusKeep, OptionData, 0x00, 0x02, 'h', 'i'}
	if string(buf) != string(wantKeep) {
		t.Fatalf("keep frame = % x, want % x", buf, wantKeep)
	}
	buf = buf[:0]
	if err := writeFrame(w, 0x1234, StatusEnd, OptionError, nil); err != nil {
		t.Fatalf("write end: %v", err)
	}
	wantEnd := []byte{0x00, 0x04, 0x12, 0x34, StatusEnd, OptionError}
	if string(buf) != string(wantEnd) {
		t.Fatalf("end frame = % x, want % x", buf, wantEnd)
	}
}

// TestWriteDataSplitsAtFrameLimit 校验超限数据按 8KiB 分帧，且每帧自洽。
func TestWriteDataSplitsAtFrameLimit(t *testing.T) {
	t.Parallel()
	payload := make([]byte, maxFrameData*2+7)
	for i := range payload {
		payload[i] = byte(i)
	}
	var buf []byte
	w := &sliceWriter{&buf}
	wr := NewWriter(w)
	n, err := wr.writeData(0x0001, payload)
	if err != nil || n != len(payload) {
		t.Fatalf("writeData = %d, %v, want %d", n, err, len(payload))
	}
	var got []byte
	r := newReader(buf)
	for {
		f, err := readFrame(r)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if f.Status != StatusKeep || f.SessionID != 0x0001 {
			t.Fatalf("unexpected frame %+v", f)
		}
		got = append(got, f.Data...)
		if len(f.Data) < maxFrameData {
			break
		}
	}
	if string(got) != string(payload) {
		t.Fatal("split frames did not reassemble to original payload")
	}
}

// echoHandler 把子连接数据原样写回，读到结束即收尾；记录开闭供断言。
type echoHandler struct {
	mu    sync.Mutex
	opens []string
	done  chan string
}

func newEchoHandler() *echoHandler { return &echoHandler{done: make(chan string, 64)} }

func (h *echoHandler) Open(target Target, conn *Conn) {
	h.mu.Lock()
	h.opens = append(h.opens, target.String())
	h.mu.Unlock()
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
	h.done <- target.String()
}

// waitDone 等待指定目标的子连接收尾。
func (h *echoHandler) waitDone(t *testing.T, target string) {
	t.Helper()
	select {
	case got := <-h.done:
		if got != target {
			t.Fatalf("handler done for %q, want %q", got, target)
		}
	case <-time.After(testTimeout):
		t.Fatalf("timeout waiting handler done for %q", target)
	}
}

// testClient 是按规范逐字节构造帧的测试客户端；读方向用生产 readFrame
// （其正确性已由 literal 用例保证）。
type testClient struct {
	t        *testing.T
	conn     net.Conn
	wmu      sync.Mutex
	next     uint16
	subs     sync.Map // uint16 → *testSub
	unknown  chan uint16
	alives   atomic.Int64
	readDone chan struct{}
}

// testSub 是客户端侧的一条子连接视图。
type testSub struct {
	id     uint16
	inbox  chan []byte
	ended  chan byte
	once   sync.Once
	failed atomic.Bool
}

func newTestClient(t *testing.T, conn net.Conn) *testClient {
	c := &testClient{t: t, conn: conn, unknown: make(chan uint16, 8), readDone: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *testClient) readLoop() {
	defer close(c.readDone)
	for {
		f, err := readFrame(c.conn)
		if err != nil {
			c.subs.Range(func(_, v any) bool {
				v.(*testSub).fail()
				return true
			})
			return
		}
		switch f.Status {
		case StatusKeep:
			if v, ok := c.subs.Load(f.SessionID); ok {
				select {
				case v.(*testSub).inbox <- f.Data:
				default:
					c.t.Errorf("sub %d inbox overflow", f.SessionID)
				}
			}
		case StatusEnd:
			if v, ok := c.subs.Load(f.SessionID); ok {
				v.(*testSub).finish(f.Option)
			} else {
				select {
				case c.unknown <- f.SessionID:
				default:
				}
			}
		case StatusKeepAlive:
			c.alives.Add(1)
		}
	}
}

func (c *testClient) write(raw []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(raw); err != nil {
		c.t.Errorf("client write: %v", err)
	}
}

// open 开子连接：发 New 帧，首数据可空（v2ray 客户端把应用首报文放在这里）。
func (c *testClient) open(target Target, first []byte) *testSub {
	return c.openRaw(c.next+1, NetworkTCP, target, first)
}

// openRaw 用指定 ID 与网络类型发 New 帧（复用 ID、UDP 拒绝等路径用）。
func (c *testClient) openRaw(id uint16, network byte, target Target, first []byte) *testSub {
	if id > c.next {
		c.next = id
	}
	sub := &testSub{id: id, inbox: make(chan []byte, 64), ended: make(chan byte, 1)}
	c.subs.Store(id, sub)
	option := byte(0)
	if len(first) > 0 {
		option = OptionData
	}
	c.write(buildFrame(id, StatusNew, option, network, target.Port, target.Host, first))
	return sub
}

// openRawUnregistered 发 New 帧但不登记（重复 ID 路径用：服务端应拒绝新帧，
// 已登记的原会话不受影响；回给该 ID 的 End 由登记方接收）。
func (c *testClient) openRawUnregistered(id uint16, network byte, target Target, first []byte) {
	option := byte(0)
	if len(first) > 0 {
		option = OptionData
	}
	c.write(buildFrame(id, StatusNew, option, network, target.Port, target.Host, first))
}

// keep 发 Keep 数据帧；data 为空时发不带数据段的 Keep。
func (c *testClient) keep(sub *testSub, data []byte) {
	option := byte(0)
	if len(data) > 0 {
		option = OptionData
	}
	c.write(buildFrame(sub.id, StatusKeep, option, 0, 0, "", data))
}

// end 发 End 帧；可带尾数据与错误标记。子连接保持登记——服务端收尾时会
// 回一个 End（正常行为，对端忽略），测试要能收到它。
func (c *testClient) end(sub *testSub, data []byte, errFlag bool) {
	option := byte(0)
	if len(data) > 0 {
		option |= OptionData
	}
	if errFlag {
		option |= OptionError
	}
	c.write(buildFrame(sub.id, StatusEnd, option, 0, 0, "", data))
}

// keepGhost 给一个从未登记的会话 ID 发 Keep 帧（陌生会话路径用）。
func (c *testClient) keepGhost(id uint16, data []byte) {
	option := byte(0)
	if len(data) > 0 {
		option = OptionData
	}
	c.write(buildFrame(id, StatusKeep, option, 0, 0, "", data))
}

// keepalive 发 KeepAlive 帧（ID 随机、数据一律丢弃）。
func (c *testClient) keepalive() {
	c.write(buildFrame(0xbeef, StatusKeepAlive, OptionData, 0, 0, "", []byte("junk")))
}

// finish 记录 End 帧（重复 End 只记第一次，对端会忽略）。
func (s *testSub) finish(option byte) {
	s.once.Do(func() { s.ended <- option })
}

// fail 标记连接已断：解开所有等待。
func (s *testSub) fail() {
	if s.failed.CompareAndSwap(false, true) {
		s.once.Do(func() { close(s.ended) })
	}
}

// readData 等待并校验一块回声数据。
func (s *testSub) readData(t *testing.T, want string) {
	t.Helper()
	select {
	case b := <-s.inbox:
		if string(b) != want {
			t.Fatalf("sub %d data = %q, want %q", s.id, b, want)
		}
	case <-time.After(testTimeout):
		t.Fatalf("sub %d timeout waiting data %q", s.id, want)
	}
}

// waitEnd 等待 End 帧并校验选项位。
func (s *testSub) waitEnd(t *testing.T, wantOption byte) {
	t.Helper()
	select {
	case opt := <-s.ended:
		if opt != wantOption {
			t.Fatalf("sub %d end option = %#x, want %#x", s.id, opt, wantOption)
		}
	case <-time.After(testTimeout):
		t.Fatalf("sub %d timeout waiting end", s.id)
	}
}

// buildFrame 按协议规范逐字节拼帧：2B 元数据长度 + 元数据 +
// （OptionData 时）2B 数据长度 + 数据。与生产 writeFrame 相互独立。
func buildFrame(id uint16, status, option, network byte, port int, host string, data []byte) []byte {
	meta := []byte{byte(id >> 8), byte(id), status, option}
	if status == StatusNew {
		meta = append(meta, network)
		meta = append(meta, byte(port>>8), byte(port))
		meta = append(meta, testAddr(host)...)
	}
	buf := make([]byte, 2, 2+len(meta)+2+len(data))
	binary.BigEndian.PutUint16(buf, uint16(len(meta)))
	buf = append(buf, meta...)
	if option&OptionData != 0 {
		buf = append(buf, byte(len(data)>>8), byte(len(data)))
		buf = append(buf, data...)
	}
	return buf
}

// testAddr 按规范编码地址段。
func testAddr(host string) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return append([]byte{addrTypeIPv4}, v4...)
		}
		return append([]byte{addrTypeIPv6}, ip.To16()...)
	}
	return append([]byte{addrTypeDomain, byte(len(host))}, host...)
}

// runServer 在 net.Pipe 的服务端一侧跑 mux.Server，返回客户端与收尾函数
// （收尾会等待服务端与客户端读协程都退出，避免测试后日志）。
func runServer(t *testing.T, h Handler) (*testClient, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	srv := NewServer(h)
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(serverConn) }()
	cli := newTestClient(t, clientConn)
	return cli, func() {
		_ = clientConn.Close()
		select {
		case <-srvDone:
		case <-time.After(testTimeout):
			t.Error("mux server did not return after connection close")
		}
		<-cli.readDone
	}
}

// TestServerEchoSubConnection 覆盖一条子连接的完整生命周期：
// New（带首数据）→ Keep → End → 服务端收尾回 End。
func TestServerEchoSubConnection(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9001}, []byte("hello"))
	sub.readData(t, "hello")
	cli.keep(sub, []byte("world"))
	sub.readData(t, "world")
	cli.end(sub, nil, false)
	sub.waitEnd(t, 0)
	h.waitDone(t, "127.0.0.1:9001")
}

// TestServerSessionIDReuseAfterEnd 断言 End 之后同一 ID 可再次使用
// （管理器在收到 End 时即摘除，不等 Handler 收尾）。
func TestServerSessionIDReuseAfterEnd(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	first := cli.openRaw(5, NetworkTCP, Target{Host: "10.0.0.1", Port: 80}, nil)
	cli.end(first, nil, false)
	first.waitEnd(t, 0)
	h.waitDone(t, "10.0.0.1:80")

	second := cli.openRaw(5, NetworkTCP, Target{Host: "10.0.0.2", Port: 80}, []byte("again"))
	second.readData(t, "again")
	cli.end(second, nil, false)
	second.waitEnd(t, 0)
	h.waitDone(t, "10.0.0.2:80")
}

// TestServerConcurrentSubConnections 并发子连接：多协程同时写回声，
// 帧互不交错；大数据按 8KiB 分帧后可完整重组。
func TestServerConcurrentSubConnections(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	targets := []Target{
		{Host: "127.0.0.1", Port: 8001},
		{Host: "example.com", Port: 443},
		{Host: "::1", Port: 8003},
	}
	subs := make([]*testSub, len(targets))
	for i, tgt := range targets {
		subs[i] = cli.open(tgt, []byte("open"))
	}
	for _, sub := range subs {
		sub.readData(t, "open")
	}

	big := make([]byte, maxFrameData*2+123)
	for i := range big {
		big[i] = byte(i)
	}
	for _, sub := range subs {
		cli.keep(sub, big)
	}
	for _, sub := range subs {
		var got []byte
		deadline := time.After(testTimeout)
		for len(got) < len(big) {
			select {
			case b := <-sub.inbox:
				got = append(got, b...)
			case <-deadline:
				t.Fatalf("sub %d short echo: %d/%d bytes", sub.id, len(got), len(big))
			}
		}
		if string(got) != string(big) {
			t.Fatalf("sub %d echo mismatch", sub.id)
		}
	}

	for _, sub := range subs {
		cli.end(sub, nil, false)
	}
	for _, sub := range subs {
		sub.waitEnd(t, 0)
	}
	for _, tgt := range targets {
		h.waitDone(t, tgt.String())
	}
}

// TestServerEndCarriesTailData 断言 End 帧附带的尾数据先于 EOF 交付，
// 回声也先于 End 帧回到客户端。
func TestServerEndCarriesTailData(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9002}, []byte("head"))
	sub.readData(t, "head")
	cli.end(sub, []byte("tail"), false)
	sub.readData(t, "tail")
	sub.waitEnd(t, 0)
	h.waitDone(t, "127.0.0.1:9002")
}

// TestServerErrorEndInterruptsInput 断言 OptionError 的 End 立即中断输入，
// 不等待缓冲排空（对端明确表示"这条子连接坏了"）。
func TestServerErrorEndInterruptsInput(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9003}, []byte("x"))
	sub.readData(t, "x")
	cli.end(sub, nil, true)
	h.waitDone(t, "127.0.0.1:9003")
}

// TestServerKeepAliveIgnored 断言 KeepAlive 帧（含携带数据）不影响任何会话，
// 之后的正常子连接照常工作。
func TestServerKeepAliveIgnored(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	cli.keepalive()
	cli.keepalive()
	sub := cli.open(Target{Host: "127.0.0.1", Port: 9004}, []byte("ping"))
	sub.readData(t, "ping")
	cli.end(sub, nil, false)
	sub.waitEnd(t, 0)
	h.waitDone(t, "127.0.0.1:9004")
}

// TestServerUnknownSessionGetsEnd 断言给陌生/已结束会话发 Keep 会收到 End
// （让对端也收尾），且不影响主连接上的其他会话。
func TestServerUnknownSessionGetsEnd(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	cli.keepGhost(777, []byte("ghost data"))
	select {
	case id := <-cli.unknown:
		if id != 777 {
			t.Fatalf("end for session %d, want 777", id)
		}
	case <-time.After(testTimeout):
		t.Fatal("no end frame for unknown session")
	}

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9005}, []byte("alive"))
	sub.readData(t, "alive")
	cli.end(sub, nil, false)
	sub.waitEnd(t, 0)
	h.waitDone(t, "127.0.0.1:9005")
}

// TestServerRejectsUDPAndBadTargets 断言 UDP 子连接与非法目标被
// End(OptionError) 拒绝，主连接保持可用。
func TestServerRejectsUDPAndBadTargets(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	udp := cli.openRaw(11, NetworkUDP, Target{Host: "127.0.0.1", Port: 53}, []byte("dns"))
	udp.waitEnd(t, OptionError)

	badPort := cli.openRaw(12, NetworkTCP, Target{Host: "127.0.0.1", Port: 0}, nil)
	badPort.waitEnd(t, OptionError)

	dup := cli.open(Target{Host: "127.0.0.1", Port: 9006}, []byte("first"))
	dup.readData(t, "first")
	cli.openRawUnregistered(dup.id, NetworkTCP, Target{Host: "127.0.0.1", Port: 9007}, nil)
	// 服务端拒绝重复 ID：回给该 ID 的 End 带 OptionError，原会话继续可用。
	dup.waitEnd(t, OptionError)
	cli.keep(dup, []byte("still here"))
	dup.readData(t, "still here")
	cli.end(dup, nil, false)
	h.waitDone(t, "127.0.0.1:9006")
}

// TestServerMalformedFrameClosesConnection 断言畸形帧让 Serve 报错返回，
// 在途子连接全部收尾（对端不可信时不能挂在半开的连接上）。
func TestServerMalformedFrameClosesConnection(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9008}, []byte("before"))
	sub.readData(t, "before")
	// metaLen 超上限的畸形帧。写方单独起协程：net.Pipe 是同步通道，服务端
	// 读完长度域就报错返回，不会再消费剩余字节，同步写会永远阻塞。
	go func() { _, _ = cli.conn.Write([]byte{0x02, 0x01, 0, 0, 0, 0, 0, 0}) }()
	h.waitDone(t, "127.0.0.1:9008")
}

// TestServerConnDeathTearsDownHandlers 断言主连接断开（无 End）时，
// 全部在途子连接的 Handler 被收尾——否则隧道会挂到客户端会话结束。
func TestServerConnDeathTearsDownHandlers(t *testing.T) {
	t.Parallel()
	h := newEchoHandler()
	cli, stop := runServer(t, h)
	defer stop()

	sub := cli.open(Target{Host: "127.0.0.1", Port: 9009}, []byte("live"))
	sub.readData(t, "live")
	// 不发 End，直接掐断主连接。
	_ = cli.conn.Close()
	h.waitDone(t, "127.0.0.1:9009")
}

// newReader 把 literal 字节包成 io.Reader。
func newReader(b []byte) *bytesReader { return &bytesReader{b: b} }

type bytesReader struct {
	b   []byte
	pos int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// sliceWriter 把写出的字节追加进一个 slice（ literal 对照用）。
type sliceWriter struct {
	dst *[]byte
}

func (w *sliceWriter) Write(p []byte) (int, error) {
	*w.dst = append(*w.dst, p...)
	return len(p), nil
}
