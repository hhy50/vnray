package core

// 用户表是唯一真源：守护"真源 → 运行时"的两条推路径（启动 seed、变更通知）——它们是限速桶与 uuid 路由唯一的灌入点。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/admin"
	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/ratelimit"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

const (
	testUUID   = "b831381d-6324-4d53-ad4f-8cda48b30811"
	testUser   = "alice"
	testClient = "edge-1"
)

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "bindings.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestRuntime 复现生产装配：绑定表 + 限速注册表 + 插件管理器 + 观察者回调。
func newTestRuntime(t *testing.T) (*admin.Bindings, *ratelimit.Registry, *plugin.Manager) {
	t.Helper()
	return newTestRuntimeWith(t, testStore(t))
}

// newTestRuntimeWith 在给定底层存储上复现生产装配（seed 测试要复用"上一轮进程留下"的库）。
func newTestRuntimeWith(t *testing.T, st store.Store) (*admin.Bindings, *ratelimit.Registry, *plugin.Manager) {
	t.Helper()
	bindings := admin.NewBindings(st)
	limits := ratelimit.NewRegistry()
	plugins := plugin.NewManager(st, bindings, transport.NewSessionManager(), limits)
	bindings.Watch(func(c admin.UserChange) { propagateUserChange(c, limits, plugins) })
	return bindings, limits, plugins
}

// TestSeedLimitsProjectsPersistedConfig 验证启动 seed 把库里的限速灌进活的注册表，否则重启即丢配额。
func TestSeedLimitsProjectsPersistedConfig(t *testing.T) {
	// 先造"上一轮进程留下的库"：直写存储，不经 Bindings，因此没有任何观察者参与。
	st := testStore(t)
	ctx := context.Background()
	for _, u := range []store.UserBinding{
		{Key: testUUID, Protocol: store.ProtocolVMess, Name: "alice-vmess",
			Status: store.UserEnabled, Client: testClient,
			Config: store.UserConfig{UpLimit: 512 * 1024, DownLimit: 256 * 1024}},
		{Key: testUser, Protocol: store.ProtocolVNV1, Name: "alice-vn-v1",
			Status: store.UserEnabled, Client: testClient,
			Config: store.UserConfig{UpLimit: 1024 * 1024}},
	} {
		if err := st.SetUser(ctx, u); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}

	// 模拟重启：全新 Bindings 从库载回，全新 Registry 是空的。
	bindings, limits, _ := newTestRuntimeWith(t, st)
	if up, down := limits.Limits(ratelimit.Identity(store.ProtocolVMess, testUUID)); up != 0 || down != 0 {
		t.Fatalf("registry should start empty, got (%d,%d)", up, down)
	}

	seedLimits(bindings, limits)

	for _, tc := range []struct {
		name     string
		p        store.Protocol
		key      string
		up, down int64
	}{
		{"vmess", store.ProtocolVMess, testUUID, 512 * 1024, 256 * 1024},
		{"vn-v1", store.ProtocolVNV1, testUser, 1024 * 1024, 0},
	} {
		up, down := limits.Limits(ratelimit.Identity(tc.p, tc.key))
		if up != tc.up || down != tc.down {
			t.Errorf("%s: limits = (%d,%d), want (%d,%d)", tc.name, up, down, tc.up, tc.down)
		}
	}
}

// TestPropagateRoutesUUIDToValidator 验证 uuid 用户在用户页增删后验证器同步跟随，杜绝"表里有、连不上"或"删了还能连"。
func TestPropagateRoutesUUIDToValidator(t *testing.T) {
	bindings, limits, plugins := newTestRuntime(t)
	p, err := plugins.Get("v2ray")
	if err != nil {
		t.Fatalf("get v2ray plugin: %v", err)
	}
	if _, ok := p.(plugin.UserSyncer); !ok {
		t.Fatal("v2ray plugin does not implement UserSyncer")
	}

	// 注册 vmess 用户 → 路由立刻可见，限速桶同步建好。
	if err := bindings.SetUser(admin.Binding{
		UserKey: testUUID, Protocol: store.ProtocolVMess, Name: "alice-vmess",
		Status: store.UserEnabled, Client: testClient,
		Config: store.UserConfig{UpLimit: 2048},
	}); err != nil {
		t.Fatalf("set vmess user: %v", err)
	}
	if routes := uuidRoutesOf(p); len(routes) != 1 || routes[0].UUID != testUUID || routes[0].Client != testClient {
		t.Fatalf("routes after set = %+v, want one entry for %s", routes, testUUID)
	}
	if up, _ := limits.Limits(ratelimit.Identity(store.ProtocolVMess, testUUID)); up != 2048 {
		t.Errorf("limit after set = %d, want 2048", up)
	}

	// vn-v1 用户不进 v2ray 路由表（协议隔离）。
	if err := bindings.SetUser(admin.Binding{
		UserKey: testUser, Protocol: store.ProtocolVNV1, Name: "alice-vn-v1",
		Status: store.UserEnabled, Client: testClient,
	}); err != nil {
		t.Fatalf("set vn-v1 user: %v", err)
	}
	if routes := uuidRoutesOf(p); len(routes) != 1 {
		t.Fatalf("vn-v1 user leaked into uuid routes: %+v", routes)
	}

	// 删除 → 路由消失（桶的回收由 ratelimit 包的 Forget 测试覆盖，此处不重复断言）。
	if _, err := bindings.DeleteUser(store.ProtocolVMess, testUUID); err != nil {
		t.Fatalf("delete vmess user: %v", err)
	}
	if routes := uuidRoutesOf(p); len(routes) != 0 {
		t.Fatalf("routes after delete = %+v, want none", routes)
	}
}

// uuidRoutesOf 取插件的 uuid 路由视图；只经窄接口读，不依赖 plugin 内部类型。
func uuidRoutesOf(p plugin.Plugin) []plugin.UUIDRoute {
	if r, ok := p.(interface{ UUIDRoutes() []plugin.UUIDRoute }); ok {
		return r.UUIDRoutes()
	}
	return nil
}
