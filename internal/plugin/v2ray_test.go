package plugin_test

// v2ray 插件的用户表接线：vmess/vless 用户经 UserStore 落库，经 SyncUser 跟随到对应
// 协议的入站验证器。用外部测试包是因为 admin 反向 import plugin，作为第三方同时引用
// 两者可避开环。

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
	"github.com/hhy50/vnray/vmess"
)

const (
	testUUID  = "b831381d-6324-4d53-ad4f-8cda48b30811"
	otherUUID = "a3f1c2d4-5e6b-4a7c-8d9e-0f1a2b3c4d5e"
)

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "plugin.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// uuidAPI 是测试要用的 v2ray 插件能力面（生产装配里由 core 通过接口断言使用）。
type uuidAPI interface {
	UUIDRoutes() []plugin.UUIDRoute
	AccountLimits(proto store.Protocol, account string) (up, down int64)
}

// newTestPlugin 装配一个只依赖绑定表与插件状态库的 v2ray 插件（不监听端口），
// 并复现 core 装配层的那条观察者：用户表变更推给对应协议的验证器。
func newTestPlugin(t *testing.T) (uuidAPI, *admin.Bindings) {
	t.Helper()
	st := testStore(t)
	b := admin.NewBindings(st)
	mgr := plugin.NewManager(st, b, nil, nil)
	b.Watch(func(c admin.UserChange) {
		p, err := mgr.Get("v2ray")
		if err != nil {
			return
		}
		if s, ok := p.(plugin.UserSyncer); ok {
			s.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
		}
	})
	p, err := mgr.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}
	api, ok := p.(uuidAPI)
	if !ok {
		t.Fatal("v2ray plugin should expose UUIDRoutes/AccountLimits")
	}
	return api, b
}

// portOf 取状态里某协议的端口；该协议不在插件职责内时返回 0。
func portOf(st plugin.Status, protocol store.Protocol) int {
	for _, kv := range st.Ports {
		if kv.Protocol == protocol {
			return kv.Port
		}
	}
	return 0
}

// freePort 占一个随机端口再释放，拿一个大概率空闲的端口号（仓库既有的竞态规避写法）。
func freePort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// assertListening 在 2 秒内轮询到端口可连为止。
func assertListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d is not listening: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUUIDUsersLiveInUserTable 断言 uuid 用户的路由与限速都读用户表（唯一真源），
// 且增删会经观察者立刻反映到插件视图。
func TestUUIDUsersLiveInUserTable(t *testing.T) {
	p, b := newTestPlugin(t)

	if n := len(p.UUIDRoutes()); n != 0 {
		t.Fatalf("初始应有 0 条路由，实际 %d", n)
	}

	if err := b.PutUser(store.UserBinding{
		Key: testUUID, Protocol: store.ProtocolVMess, Name: "alice-vmess",
		Status: store.UserEnabled, Client: "edge-1",
		Config: store.UserConfig{UpLimit: 2 << 20, DownLimit: 512 << 10},
	}); err != nil {
		t.Fatalf("put user: %v", err)
	}
	routes := p.UUIDRoutes()
	if len(routes) != 1 || routes[0].UUID != testUUID || routes[0].Client != "edge-1" ||
		routes[0].Protocol != store.ProtocolVMess {
		t.Fatalf("路由 = %+v", routes)
	}
	if up, down := p.AccountLimits(store.ProtocolVMess, testUUID); up != 2<<20 || down != 512<<10 {
		t.Fatalf("限速 = %d/%d, want %d/%d", up, down, 2<<20, 512<<10)
	}

	// 删除后路由立刻消失。
	if _, err := b.DeleteUser(store.ProtocolVMess, testUUID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := len(p.UUIDRoutes()); n != 0 {
		t.Fatalf("删除后仍有 %d 条路由", n)
	}
	if up, _ := p.AccountLimits(store.ProtocolVMess, testUUID); up != 0 {
		t.Fatalf("删除后限速 = %d, want 0", up)
	}
}

// TestSameKeyCoexistsAcrossProtocols 断言协议是身份的一部分：同一个 uuid 可以在 vmess 与
// vless 各有一条记录，删掉其中一条不影响另一条。
func TestSameKeyCoexistsAcrossProtocols(t *testing.T) {
	p, b := newTestPlugin(t)
	for _, protocol := range []store.Protocol{store.ProtocolVMess, store.ProtocolVLESS} {
		if err := b.PutUser(store.UserBinding{Key: testUUID, Protocol: protocol,
			Name: "alice-" + string(protocol), Status: store.UserEnabled, Client: "edge-1"}); err != nil {
			t.Fatalf("put %s user: %v", protocol, err)
		}
	}
	if routes := p.UUIDRoutes(); len(routes) != 2 {
		t.Fatalf("routes = %+v, want vmess 与 vless 各一条", routes)
	}
	if _, err := b.DeleteUser(store.ProtocolVMess, testUUID); err != nil {
		t.Fatalf("delete vmess user: %v", err)
	}
	routes := p.UUIDRoutes()
	if len(routes) != 1 || routes[0].Protocol != store.ProtocolVLESS {
		t.Fatalf("routes after delete = %+v, want 只剩 vless", routes)
	}
}

// TestNonUUIDProtocolsAreNotV2RayRoutes 断言非 vmess/vless 的记录不会混进 v2ray 路由，
// 各协议同表但靠 protocol 严格区分。
func TestNonUUIDProtocolsAreNotV2RayRoutes(t *testing.T) {
	p, b := newTestPlugin(t)
	if err := b.PutUser(store.UserBinding{
		Key: testUUID, Protocol: store.ProtocolVNV1, Name: "alice-vn-v1",
		Status: store.UserEnabled, Client: "edge-1",
	}); err != nil {
		t.Fatalf("put user: %v", err)
	}
	if err := b.PutUser(store.UserBinding{
		Key: otherUUID, Protocol: store.ProtocolVMess, Name: "bob-vmess",
		Status: store.UserEnabled, Client: "edge-2",
	}); err != nil {
		t.Fatalf("put user: %v", err)
	}
	routes := p.UUIDRoutes()
	if len(routes) != 1 || routes[0].UUID != otherUUID {
		t.Fatalf("路由应只含 vmess/vless 的记录，实际 %+v", routes)
	}
	if up, _ := p.AccountLimits(store.ProtocolVNV1, testUUID); up != 0 {
		t.Fatal("vn-v1 的记录被当成了 v2ray 账号")
	}
}

// TestRouteChangeKeepsConfig 断言改路由不丢同行的限速配置（两者同行不同列）。
func TestRouteChangeKeepsConfig(t *testing.T) {
	p, b := newTestPlugin(t)
	cfg := store.UserConfig{UpLimit: 1 << 20, DownLimit: 1 << 20}
	if err := b.PutUser(store.UserBinding{
		Key: testUUID, Protocol: store.ProtocolVMess, Name: "alice-vmess",
		Status: store.UserEnabled, Client: "edge-1", Config: cfg,
	}); err != nil {
		t.Fatalf("put user: %v", err)
	}
	// 管理台改绑：读回原行、只换 client、原样写回 config。
	cur, ok := b.LookupUser(store.ProtocolVMess, testUUID)
	if !ok {
		t.Fatal("lookup user failed")
	}
	cur.Client = "edge-9"
	if err := b.PutUser(cur); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	routes := p.UUIDRoutes()
	if len(routes) != 1 || routes[0].Client != "edge-9" {
		t.Fatalf("改绑后路由 = %+v", routes)
	}
	if up, down := p.AccountLimits(store.ProtocolVMess, testUUID); up != 1<<20 || down != 1<<20 {
		t.Fatalf("改绑后限速丢了: %d/%d", up, down)
	}
}

// TestValidateUserKey 断言添加用户时的密钥格式校验：vmess/vless 用户的密钥是 uuid，
// 须为合法 UUID（格式不对会出现"表里有、连不上"）；vn-v1 与 ss 只要求非空。
func TestValidateUserKey(t *testing.T) {
	if err := plugin.ValidateUserKey(store.ProtocolVNV1, "any-key"); err != nil {
		t.Fatalf("vn-v1 key: %v", err)
	}
	if err := plugin.ValidateUserKey(store.ProtocolSS, "any-password"); err != nil {
		t.Fatalf("ss key: %v", err)
	}
	for _, protocol := range []store.Protocol{store.ProtocolVMess, store.ProtocolVLESS} {
		if err := plugin.ValidateUserKey(protocol, testUUID); err != nil {
			t.Fatalf("%s valid uuid: %v", protocol, err)
		}
		if err := plugin.ValidateUserKey(protocol, "not-a-uuid"); err == nil {
			t.Fatalf("%s should reject a malformed uuid", protocol)
		}
	}
}

// TestSetPortsWhileStopped 断言停用态改端口只落盘、不起监听：Status 反映待启用端口，
// vn_plugin 里同步更新（下次启用与重启恢复都用它）。
func TestSetPortsWhileStopped(t *testing.T) {
	st := testStore(t)
	b := admin.NewBindings(st)
	mgr := plugin.NewManager(st, b, nil, nil)
	p, err := mgr.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}
	setter, ok := p.(plugin.MultiPortSetter)
	if !ok {
		t.Fatal("v2ray plugin should implement plugin.MultiPortSetter")
	}
	ctx := context.Background()
	ports := map[store.Protocol]int{store.ProtocolVMess: 39117, store.ProtocolVLESS: 39118}
	if err := setter.SetPorts(ctx, ports); err != nil {
		t.Fatalf("set ports: %v", err)
	}
	got := p.Status()
	if got.Enabled {
		t.Fatalf("status = %+v, want stopped", got)
	}
	if vmess, vless := portOf(got, store.ProtocolVMess), portOf(got, store.ProtocolVLESS); vmess != 39117 || vless != 39118 {
		t.Fatalf("status ports = %d/%d, want 39117/39118", vmess, vless)
	}
	state, ok, err := st.GetPlugin(ctx, "v2ray")
	if err != nil || !ok {
		t.Fatalf("get plugin = (%+v, %v, %v), want found", state, ok, err)
	}
	if state.Config.Ports[store.ProtocolVMess] != 39117 || state.Config.Ports[store.ProtocolVLESS] != 39118 {
		t.Fatalf("stored ports = %v, want 39117/39118", state.Config.Ports)
	}
	if err := setter.SetPorts(ctx, map[store.Protocol]int{store.ProtocolVMess: 0}); err == nil {
		t.Fatal("port 0 should be rejected")
	}
}

// TestRestoreKeepsPortsWhileDisabled 回归：停用态的插件重启后仍要认得上次配的端口。
// 丢了端口，管理台端口列会退回默认值，用户页的分享链接也跟着变。
func TestRestoreKeepsPortsWhileDisabled(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	mgr := plugin.NewManager(st, admin.NewBindings(st), nil, nil)
	p, err := mgr.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}
	ports := map[store.Protocol]int{store.ProtocolVMess: 39119, store.ProtocolVLESS: 39120}
	if err := p.(plugin.MultiPortSetter).SetPorts(ctx, ports); err != nil {
		t.Fatalf("set ports: %v", err)
	}

	// 换一个 Manager 复用同一份存储，等价于进程重启。
	restarted, err := plugin.NewManager(st, admin.NewBindings(st), nil, nil).Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray after restart: %v", err)
	}
	restarted.Restore(ctx)
	got := restarted.Status()
	if got.Enabled {
		t.Fatalf("status after restore = %+v, want stopped", got)
	}
	if vmess, vless := portOf(got, store.ProtocolVMess), portOf(got, store.ProtocolVLESS); vmess != 39119 || vless != 39120 {
		t.Fatalf("status ports after restore = %d/%d, want 39119/39120", vmess, vless)
	}
}

// vlessProbe 往 vless 入站发一个明文请求头（version0 + uuid16 + addonsLen0 + cmd TCP +
// port 80 + addrType IPv4 + 127.0.0.1），返回读到的前两个字节（响应头）。
// 认证被拒时入站不写响应直接关连接，读会以错误结束。
func vlessProbe(port int, raw [16]byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	head := []byte{0x00}
	head = append(head, raw[:]...)
	head = append(head, 0x00, 0x01, 0x00, 0x50, 0x01, 127, 0, 0, 1)
	if _, err := conn.Write(head); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// TestDisabledUserIsRejectedByInbound 断言停用即时生效：同一个 uuid 停用前能通过 vless
// 入站认证，停用后立刻被拒——不必等重启，重启后也不会被重新装回（uuidRoutes 只含启用用户）。
func TestDisabledUserIsRejectedByInbound(t *testing.T) {
	st := testStore(t)
	b := admin.NewBindings(st)
	mgr := plugin.NewManager(st, b, transport.NewSessionManager(), nil)
	b.Watch(func(c admin.UserChange) {
		p, err := mgr.Get("v2ray")
		if err != nil {
			return
		}
		if s, ok := p.(plugin.UserSyncer); ok {
			s.SyncUser(c.Protocol, c.Key, c.Client, c.Status, c.Deleted)
		}
	})
	p, err := mgr.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}
	vmessPort, vlessPort := freePort(t), freePort(t)
	if err := p.Enable(context.Background(), map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	t.Cleanup(func() { _ = p.Disable(context.Background()) })
	assertListening(t, vlessPort)

	raw, err := vmess.ParseUUID(testUUID)
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	put := func(status store.UserStatus) {
		t.Helper()
		if err := b.PutUser(store.UserBinding{Key: testUUID, Protocol: store.ProtocolVLESS,
			Name: "alice-vless", Status: status, Client: "edge-1"}); err != nil {
			t.Fatalf("put user: %v", err)
		}
	}

	put(store.UserEnabled)
	got, err := vlessProbe(vlessPort, raw)
	if err != nil || string(got) != "\x00\x00" {
		t.Fatalf("启用时应通过认证并收到响应头: got=%q err=%v", got, err)
	}

	put(store.UserDisabled)
	if got, err := vlessProbe(vlessPort, raw); err == nil {
		t.Fatalf("停用后仍通过了认证: got=%q", got)
	}
}

// TestEnableSurvivesRequestContext 回归：管理台的 Enable/SetPorts 带的是 HTTP 请求 ctx，
// 请求返回即取消——监听器不能绑在它上面，否则插件显示"运行中"但端口无人监听。
func TestEnableSurvivesRequestContext(t *testing.T) {
	st := testStore(t)
	b := admin.NewBindings(st)
	mgr := plugin.NewManager(st, b, nil, nil)
	p, err := mgr.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray: %v", err)
	}

	vmessPort, vlessPort := freePort(t), freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Enable(ctx, map[string]string{
		"vmess_port": strconv.Itoa(vmessPort),
		"vless_port": strconv.Itoa(vlessPort),
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	cancel() // 模拟管理台 HTTP 请求结束
	t.Cleanup(func() { _ = p.Disable(context.Background()) })

	assertListening(t, vmessPort)
	assertListening(t, vlessPort)
	if got := p.Status(); !got.Enabled {
		t.Fatalf("status = %+v, want enabled with both inbounds up", got)
	}
}
