package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// testMySQLDSN 由环境变量提供；MySQL 依赖外部服务，CI 与本机默认跳过。
func testMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TEST_MYSQL_DSN (e.g. root:pass@tcp(127.0.0.1:3306)/dbname) to run mysql tests")
	}
	return dsn
}

// sqliteStore 在临时目录里开一个文件库；sqlite 的 ":memory:" 每个连接是独立库，
// 用文件而不是共享缓存，保证行为与生产一致。
func sqliteStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(context.Background(), "sqlite", filepath.Join(dir, "bindings.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// drivers 返回各驱动的构造器：sqlite（临时文件）始终运行，MySQL 由 TEST_MYSQL_DSN 门控。
func drivers(t *testing.T) map[string]func(t *testing.T) Store {
	t.Helper()
	return map[string]func(t *testing.T) Store{
		"sqlite": sqliteStore,
		"mysql": func(t *testing.T) Store {
			st, err := Open(context.Background(), "mysql", testMySQLDSN(t))
			if err != nil {
				t.Fatalf("open mysql: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st
		},
	}
}

// TestUserBindingConformance 是三种驱动共用的用户表行为契约：Set 覆盖（保留 id/created_at）、
// List 按 (type, key) 有序、Delete 幂等、非法记录被拒、config 往返。
func TestUserBindingConformance(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)

			// 同名 key 在不同协议下是两条独立记录。
			mustSetUser(t, st, UserBinding{Key: "alice", Protocol: ProtocolVNV1, Name: "Alice",
				Status: UserEnabled, Client: "edge-1",
				Config: UserConfig{UpLimit: 1024, DownLimit: 2048}})
			mustSetUser(t, st, UserBinding{Key: "alice", Protocol: ProtocolVMess, Name: "Alice",
				Status: UserEnabled, Client: "edge-2"})
			mustSetUser(t, st, UserBinding{Key: "bob", Protocol: ProtocolVNV1, Name: "Bob",
				Status: UserEnabled, Client: "edge-1"})

			got, ok, err := st.GetUser(ctx, ProtocolVMess, "alice")
			if err != nil || !ok {
				t.Fatalf("get vmess/alice = (%+v, %v, %v), want found", got, ok, err)
			}
			if got.Client != "edge-2" {
				t.Errorf("vmess/alice client = %q, want edge-2", got.Client)
			}
			if got.ID == 0 || got.CreatedAt.IsZero() {
				t.Errorf("vmess/alice must carry id and created_at, got id=%d created=%v", got.ID, got.CreatedAt)
			}

			// 覆盖写：策略是换 client 与 config，id 与 created_at 必须保留。
			first, _, _ := st.GetUser(ctx, ProtocolVNV1, "alice")
			mustSetUser(t, st, UserBinding{Key: "alice", Protocol: ProtocolVNV1, Name: "Alice",
				Status: UserEnabled, Client: "edge-9",
				Config: UserConfig{UpLimit: 4096}})
			second, _, _ := st.GetUser(ctx, ProtocolVNV1, "alice")
			if second.Client != "edge-9" {
				t.Errorf("after overwrite client = %q, want edge-9", second.Client)
			}
			if second.ID != first.ID {
				t.Errorf("overwrite changed id: %d → %d", first.ID, second.ID)
			}
			if !second.CreatedAt.Equal(first.CreatedAt) {
				t.Errorf("overwrite changed created_at: %v → %v", first.CreatedAt, second.CreatedAt)
			}
			if second.Config.UpLimit != 4096 {
				t.Errorf("after overwrite config = %+v, want up=4096", second.Config)
			}

			// List 按 (protocol, key) 升序：vmess 排在 vn-v1 之前。
			items, err := st.ListUsers(ctx)
			if err != nil {
				t.Fatalf("list users: %v", err)
			}
			want := []string{string(ProtocolVMess) + "/alice", string(ProtocolVNV1) + "/alice", string(ProtocolVNV1) + "/bob"}
			gotOrder := make([]string, 0, len(items))
			for _, u := range items {
				gotOrder = append(gotOrder, string(u.Protocol)+"/"+u.Key)
			}
			if len(gotOrder) != len(want) {
				t.Fatalf("list users = %v, want %v", gotOrder, want)
			}
			for i := range want {
				if gotOrder[i] != want[i] {
					t.Errorf("list users[%d] = %q, want %q (order must be protocol, key asc)", i, gotOrder[i], want[i])
				}
			}

			// Delete 幂等。
			deleted, err := st.DeleteUser(ctx, ProtocolVNV1, "bob")
			if err != nil || !deleted {
				t.Fatalf("delete bob = (%v, %v), want (true, nil)", deleted, err)
			}
			deleted, err = st.DeleteUser(ctx, ProtocolVNV1, "bob")
			if err != nil || deleted {
				t.Fatalf("delete bob again = (%v, %v), want (false, nil)", deleted, err)
			}
			// 删 vn-v1/alice 不能顺手删掉 vmess/alice。
			if _, err := st.DeleteUser(ctx, ProtocolVNV1, "alice"); err != nil {
				t.Fatalf("delete vn-v1/alice: %v", err)
			}
			if _, ok, _ := st.GetUser(ctx, ProtocolVMess, "alice"); !ok {
				t.Error("deleting vn-v1/alice must not remove vmess/alice")
			}

			// 非法记录被拒。
			if err := st.SetUser(ctx, UserBinding{Key: "", Protocol: ProtocolVNV1, Name: "n",
				Status: UserEnabled, Client: "x"}); err == nil {
				t.Error("set user with empty key: want error, got nil")
			}
			if err := st.SetUser(ctx, UserBinding{Key: "k", Protocol: "bogus", Name: "n",
				Status: UserEnabled, Client: "x"}); err == nil {
				t.Error("set user with unknown protocol: want error, got nil")
			}
			if err := st.SetUser(ctx, UserBinding{Key: "k", Protocol: ProtocolVNV1, Name: "",
				Status: UserEnabled, Client: "x"}); err == nil {
				t.Error("set user with empty name: want error, got nil")
			}
			if err := st.SetUser(ctx, UserBinding{Key: "k", Protocol: ProtocolVNV1, Name: "n",
				Status: UserEnabled, Client: ""}); err == nil {
				t.Error("set user with empty client: want error, got nil")
			}
			if err := st.SetUser(ctx, UserBinding{Key: "k", Protocol: ProtocolVNV1, Name: "n",
				Status: 2, Client: "x"}); err == nil {
				t.Error("set user with status 2: want error, got nil")
			}
			if _, ok, _ := st.GetUser(ctx, ProtocolVNV1, "missing"); ok {
				t.Error("get missing user: want ok=false")
			}
		})
	}
}

// TestParseClientConfig 覆盖 server_config 里服务端认识的字段解析（含自定义 host 与 IP 段映射）。
func TestParseClientConfig(t *testing.T) {
	cfg := ParseClientConfig(`{"log_level":"debug","concurrency":64,"custom_hosts":[{"domain":"a.example","ip":"10.0.0.1"}],"ip_maps":[{"from":"123.123.*.*","to":"192.168.*.*"}]}`)
	if cfg.LogLevel != "debug" {
		t.Fatalf("log_level = %q, want debug", cfg.LogLevel)
	}
	if cfg.Concurrency != 64 {
		t.Fatalf("concurrency = %d, want 64", cfg.Concurrency)
	}
	if len(cfg.CustomHosts) != 1 || cfg.CustomHosts[0].Domain != "a.example" {
		t.Fatalf("custom_hosts = %+v", cfg.CustomHosts)
	}
	if len(cfg.IPMaps) != 1 || cfg.IPMaps[0].From != "123.123.*.*" || cfg.IPMaps[0].To != "192.168.*.*" {
		t.Fatalf("ip_maps = %+v", cfg.IPMaps)
	}
	if empty := ParseClientConfig(""); empty.LogLevel != "" || empty.CustomHosts != nil || empty.IPMaps != nil || empty.Concurrency != 0 {
		t.Fatalf("empty parse = %+v, want zero value", empty)
	}
	if bad := ParseClientConfig("not json"); bad.LogLevel != "" || bad.CustomHosts != nil || bad.IPMaps != nil || bad.Concurrency != 0 {
		t.Fatalf("bad json parse = %+v, want zero value", bad)
	}
}

// TestUserConfigConcurrency 覆盖 UserConfig 并发字段的编码往返与零值判断。
func TestUserConfigConcurrency(t *testing.T) {
	cfg := ParseUserConfig(`{"up_limit":1024,"concurrency":32}`)
	if cfg.Concurrency != 32 {
		t.Fatalf("concurrency = %d, want 32", cfg.Concurrency)
	}
	if enc := (UserConfig{UpLimit: 10}).Encode(); enc != `{"up_limit":10}` {
		t.Fatalf("encode without concurrency = %q", enc)
	}
	if (UserConfig{}).Encode() != "" {
		t.Fatal("all-zero config should encode to empty string")
	}
	if (UserConfig{Concurrency: 5}).Encode() == "" {
		t.Fatal("config with concurrency must not encode to empty string")
	}
}

// TestClientBindingConformance 覆盖客户端注册表：同名单行（upsert 保留 id）、有序、删除幂等。
func TestClientBindingConformance(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)

			mustSetClient(t, st, ClientBinding{Key: "edge-2", Type: ClientTypeProxy, Config: `{"note":"b"}`})
			mustSetClient(t, st, ClientBinding{Key: "edge-1", Type: ClientTypeProxy})

			// 同名重连：只更新一行，不新增，id 与 created_at 保留。
			first, ok, err := st.GetClient(ctx, "edge-2")
			if err != nil || !ok {
				t.Fatalf("get edge-2 = (%+v, %v, %v), want found", first, ok, err)
			}
			// 上报快照与运维配置分列：写入其一不得覆盖另一个。
			mustSetClient(t, st, ClientBinding{Key: "edge-2", Type: ClientTypeProxy,
				Config: `{"note":"b2"}`, ServerConfig: `{"log_level":"warn"}`})
			second, _, _ := st.GetClient(ctx, "edge-2")
			if second.ID != first.ID {
				t.Errorf("reconnect changed id: %d → %d", first.ID, second.ID)
			}
			if second.Config != `{"note":"b2"}` {
				t.Errorf("reconnect config = %q, want updated", second.Config)
			}
			if second.ServerConfig != `{"log_level":"warn"}` {
				t.Errorf("reconnect server_config = %q, want kept", second.ServerConfig)
			}
			mustSetClient(t, st, ClientBinding{Key: "edge-2", Type: ClientTypeProxy,
				ServerConfig: `{"log_level":"info"}`})
			third, _, _ := st.GetClient(ctx, "edge-2")
			// Store 是全量 upsert：没传的列就是清空，两列的"保留"语义由 admin.Bindings
			// 的读改写保证（见 admin 包的 SaveClientReportedConfig 测试）。
			if third.ServerConfig != `{"log_level":"info"}` || third.Config != "" {
				t.Fatalf("after server-side update: config=%q server_config=%q, want config cleared",
					third.Config, third.ServerConfig)
			}

			clients, err := st.ListClients(ctx)
			if err != nil {
				t.Fatalf("list clients: %v", err)
			}
			if len(clients) != 2 {
				t.Fatalf("list clients = %d rows, want 2 (same name must upsert)", len(clients))
			}
			if clients[0].Key != "edge-1" || clients[1].Key != "edge-2" {
				t.Errorf("clients order = %q,%q want edge-1,edge-2", clients[0].Key, clients[1].Key)
			}

			deleted, err := st.DeleteClient(ctx, "edge-1")
			if err != nil || !deleted {
				t.Fatalf("delete edge-1 = (%v, %v), want (true, nil)", deleted, err)
			}
			deleted, err = st.DeleteClient(ctx, "edge-1")
			if err != nil || deleted {
				t.Fatalf("delete edge-1 again = (%v, %v), want (false, nil)", deleted, err)
			}
			if err := st.SetClient(ctx, ClientBinding{Key: "", Type: ClientTypeProxy}); err == nil {
				t.Error("set client with empty key: want error, got nil")
			}
		})
	}
}

// TestPluginStateTable 断言 vn_plugin 与用户/客户端表互不干扰（插件状态不落在用户表里），
// 按 name 覆盖写，且 Config 的 JSON 往返正确。
func TestPluginStateTable(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)

			if err := st.SetPlugin(ctx, PluginState{Name: "v2ray", Status: PluginEnabled,
				Config: PluginConfig{Ports: map[Protocol]int{ProtocolVMess: 11120, ProtocolVLESS: 11121}}}); err != nil {
				t.Fatalf("set plugin: %v", err)
			}
			got, ok, err := st.GetPlugin(ctx, "v2ray")
			if err != nil || !ok {
				t.Fatalf("get plugin = (%+v, %v, %v), want found", got, ok, err)
			}
			if got.Status != PluginEnabled {
				t.Errorf("plugin status = %q, want %q", got.Status, PluginEnabled)
			}
			if got.Config.Ports[ProtocolVMess] != 11120 || got.Config.Ports[ProtocolVLESS] != 11121 {
				t.Errorf("plugin ports = %v, want vmess=11120 vless=11121", got.Config.Ports)
			}

			// 覆盖写：只改 status，Config 原样保留。
			if err := st.SetPlugin(ctx, PluginState{Name: "v2ray", Status: PluginDisabled, Config: got.Config}); err != nil {
				t.Fatalf("overwrite plugin: %v", err)
			}
			if again, _, _ := st.GetPlugin(ctx, "v2ray"); again.Status != PluginDisabled ||
				again.Config.Ports[ProtocolVLESS] != 11121 {
				t.Errorf("plugin after overwrite = %+v, want disabled with ports kept", again)
			}

			if err := st.SetPlugin(ctx, PluginState{Name: "ss", Status: PluginDisabled,
				Config: PluginConfig{Method: "aes-256-gcm", Ports: map[Protocol]int{ProtocolSS: 9000}}}); err != nil {
				t.Fatalf("set ss plugin: %v", err)
			}
			all, err := st.ListPlugins(ctx)
			if err != nil {
				t.Fatalf("list plugins: %v", err)
			}
			if len(all) != 2 || all[0].Name != "ss" || all[1].Name != "v2ray" {
				t.Fatalf("list plugins = %+v, want [ss v2ray] in name order", all)
			}
			if users, _ := st.ListUsers(ctx); len(users) != 0 {
				t.Errorf("plugin writes must not appear in user table, got %v", users)
			}
			if _, ok, _ := st.GetPlugin(ctx, "missing"); ok {
				t.Error("get missing plugin: want ok=false")
			}
		})
	}
}

// TestOpenRejectsUnknownDriver 断言未知驱动与缺失 dsn 在打开阶段就报错。
func TestOpenRejectsUnknownDriver(t *testing.T) {
	if _, err := Open(context.Background(), "postgres", "x"); err == nil {
		t.Fatal("unknown driver: want error, got nil")
	}
	if _, err := Open(context.Background(), "mysql", ""); err == nil {
		t.Fatal("mysql without dsn: want error, got nil")
	}
}

func mustSetUser(t *testing.T, st Store, b UserBinding) {
	t.Helper()
	if err := st.SetUser(context.Background(), b); err != nil {
		t.Fatalf("set user %s/%s: %v", b.Protocol, b.Key, err)
	}
}

func mustSetClient(t *testing.T, st Store, b ClientBinding) {
	t.Helper()
	if err := st.SetClient(context.Background(), b); err != nil {
		t.Fatalf("set client %s: %v", b.Key, err)
	}
}

// TestTrafficConformance 是三种驱动共用的流量表契约：增量累加而不是覆盖、
// 同名 key 按协议分开记、零增量不建行、删除幂等。
func TestTrafficConformance(t *testing.T) {
	for name, open := range drivers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)

			if err := st.AddUserTraffic(ctx, ProtocolVNV1, "alice", 100, 200); err != nil {
				t.Fatalf("add user traffic: %v", err)
			}
			if err := st.AddUserTraffic(ctx, ProtocolVNV1, "alice", 1, 2); err != nil {
				t.Fatalf("add user traffic again: %v", err)
			}
			if err := st.AddUserTraffic(ctx, ProtocolSS, "alice", 5, 6); err != nil {
				t.Fatalf("add ss traffic: %v", err)
			}
			rows, err := st.ListUserTraffic(ctx)
			if err != nil {
				t.Fatalf("list user traffic: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("user traffic rows = %d, want 2", len(rows))
			}
			for _, r := range rows {
				switch r.Protocol {
				case ProtocolVNV1:
					if r.Up != 101 || r.Down != 202 {
						t.Errorf("vn-v1 traffic = %d/%d, want 101/202", r.Up, r.Down)
					}
				case ProtocolSS:
					if r.Up != 5 || r.Down != 6 {
						t.Errorf("ss traffic = %d/%d, want 5/6", r.Up, r.Down)
					}
				}
			}

			// 零增量不该建行。
			if err := st.AddClientTraffic(ctx, "edge-1", 0, 0); err != nil {
				t.Fatalf("add zero client traffic: %v", err)
			}
			if rows, _ := st.ListClientTraffic(ctx); len(rows) != 0 {
				t.Fatalf("zero increment created a row: %v", rows)
			}
			if err := st.AddClientTraffic(ctx, "edge-1", 7, 8); err != nil {
				t.Fatalf("add client traffic: %v", err)
			}
			if err := st.AddClientTraffic(ctx, "edge-1", 1, 1); err != nil {
				t.Fatalf("add client traffic again: %v", err)
			}
			if rows, _ := st.ListClientTraffic(ctx); len(rows) != 1 || rows[0].Up != 8 || rows[0].Down != 9 {
				t.Fatalf("client traffic = %v, want one row 8/9", rows)
			}

			deleted, err := st.DeleteUserTraffic(ctx, ProtocolVNV1, "alice")
			if err != nil || !deleted {
				t.Fatalf("delete user traffic = (%v, %v), want (true, nil)", deleted, err)
			}
			if deleted, _ := st.DeleteUserTraffic(ctx, ProtocolVNV1, "alice"); deleted {
				t.Error("second delete reported true")
			}
			if deleted, err := st.DeleteClientTraffic(ctx, "edge-1"); err != nil || !deleted {
				t.Fatalf("delete client traffic = (%v, %v), want (true, nil)", deleted, err)
			}
		})
	}
}
