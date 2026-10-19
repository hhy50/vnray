package store

// portmapping_test.go — port_mappings 表的 CRUD 与 remote_port 唯一性契约。

import (
	"context"
	"strings"
	"testing"
)

func TestPortMappingConformance(t *testing.T) {
	st := sqliteStore(t)
	ctx := context.Background()

	m := PortMapping{Client: "ns:grp:edge-1", RemotePort: 7000,
		LocalHost: "127.0.0.1", LocalPort: 8080, Enabled: true, Remark: "web"}
	if err := st.SetPortMapping(ctx, m); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := st.ListPortMappings(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list after insert: rows=%d err=%v", len(rows), err)
	}
	id := rows[0].ID
	if id == 0 || rows[0].RemotePort != 7000 || !rows[0].Enabled || rows[0].Remark != "web" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if rows[0].CreatedAt.IsZero() || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", rows[0])
	}

	// 按 ID 覆盖：改本地端口与启用态，保留 CreatedAt。
	saved, ok, err := st.GetPortMapping(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	saved.LocalPort = 9090
	saved.Enabled = false
	if err := st.SetPortMapping(ctx, saved); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, err := st.GetPortMapping(ctx, id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.LocalPort != 9090 || got.Enabled || !got.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("unexpected row after update: %+v (created %v)", got, saved.CreatedAt)
	}

	// 限速字段（字节/秒）随覆盖更新；负值在校验层拒绝。
	saved.Config = PortConfig{UpLimit: 512 * 1024, DownLimit: 256 * 1024}
	if err := st.SetPortMapping(ctx, saved); err != nil {
		t.Fatalf("update limits: %v", err)
	}
	got, _, _ = st.GetPortMapping(ctx, id)
	if got.Config.UpLimit != 512*1024 || got.Config.DownLimit != 256*1024 {
		t.Fatalf("limits not persisted: %+v", got)
	}
	// 落库形态与 user_bindings.config 的 UserConfig 统一：config JSON 列、同名键。
	src, okSrc := st.(GORMSource)
	if !okSrc {
		t.Fatal("store does not expose GORMSource")
	}
	var rawConfig string
	if err := src.GORM().Table("vn_port_mappings").Select("config").
		Where("id = ?", id).Scan(&rawConfig).Error; err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if !strings.Contains(rawConfig, `"up_limit":524288`) ||
		!strings.Contains(rawConfig, `"down_limit":262144`) {
		t.Fatalf("config column = %q, want UserConfig-compatible keys", rawConfig)
	}
	neg := PortMapping{Client: "ns:grp:x", RemotePort: 7002, LocalHost: "127.0.0.1", LocalPort: 80,
		Config: PortConfig{UpLimit: -1}}
	if err := st.SetPortMapping(ctx, neg); err == nil {
		t.Fatal("negative limit must fail")
	}

	// remote_port 全表唯一：与其他行冲突必须报错（无论插入还是改既有行）。
	dup := PortMapping{Client: "ns:grp:edge-2", RemotePort: 7000,
		LocalHost: "127.0.0.1", LocalPort: 22, Enabled: true}
	if err := st.SetPortMapping(ctx, dup); err == nil {
		t.Fatal("insert with duplicate remote port must fail")
	}
	other := PortMapping{Client: "ns:grp:edge-2", RemotePort: 7001,
		LocalHost: "127.0.0.1", LocalPort: 22, Enabled: true}
	if err := st.SetPortMapping(ctx, other); err != nil {
		t.Fatalf("insert second mapping: %v", err)
	}
	rows2, _ := st.ListPortMappings(ctx)
	var otherID uint64
	for _, r := range rows2 {
		if r.RemotePort == 7001 {
			otherID = r.ID
		}
	}
	other.RemotePort = 7000
	if err := st.SetPortMapping(ctx, other); err == nil {
		t.Fatal("update onto existing remote port must fail")
	}

	// 更新不存在的 ID 报错；删除返回存在性。
	missing := PortMapping{ID: 99999, Client: "ns:grp:x", RemotePort: 7100,
		LocalHost: "127.0.0.1", LocalPort: 80}
	if err := st.SetPortMapping(ctx, missing); err == nil {
		t.Fatal("update nonexistent id must fail")
	}
	if deleted, err := st.DeletePortMapping(ctx, otherID); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := st.DeletePortMapping(ctx, otherID); err != nil || deleted {
		t.Fatalf("delete twice: deleted=%v err=%v", deleted, err)
	}

	// 校验：客户端、端口范围、本地主机缺一不可。
	for _, bad := range []PortMapping{
		{RemotePort: 7002, LocalHost: "127.0.0.1", LocalPort: 80},
		{Client: "ns:grp:x", RemotePort: 0, LocalHost: "127.0.0.1", LocalPort: 80},
		{Client: "ns:grp:x", RemotePort: 7002, LocalPort: 80},
		{Client: "ns:grp:x", RemotePort: 7002, LocalHost: "127.0.0.1", LocalPort: 0},
	} {
		if err := st.SetPortMapping(ctx, bad); err == nil {
			t.Fatalf("invalid mapping must fail: %+v", bad)
		}
	}
}
