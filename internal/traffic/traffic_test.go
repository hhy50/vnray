package traffic

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

// testStore 在临时目录开一个 sqlite 库，测试结束自动关闭。
func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "traffic.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestFlushWritesDeltasOnce 断言同一份增量只落库一次，且用户与客户端两个维度都记。
func TestFlushWritesDeltasOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")

	user := m.UserStats(store.ProtocolVNV1, "alice")
	user.UpWrite.Add(100)
	user.DownWrite.Add(200)
	client := m.ClientStats("edge-1")
	client.UpWrite.Add(100)
	client.DownWrite.Add(200)

	m.Flush(ctx, st)
	assertUserTraffic(t, st, 100, 200)
	assertClientTraffic(t, st, 100, 200)

	// 没有新流量：再 flush 不得重复累加。
	m.Flush(ctx, st)
	assertUserTraffic(t, st, 100, 200)
	assertClientTraffic(t, st, 100, 200)

	// 同一身份的多条隧道必须共用一份计数器，否则并发隧道会各记一份。
	if got := m.UserStats(store.ProtocolVNV1, "alice"); got != user {
		t.Fatal("same identity got a different counter")
	}
	if got := m.ClientStats("edge-1"); got != client {
		t.Fatal("same client got a different counter")
	}

	user.UpWrite.Add(50)
	m.Flush(ctx, st)
	assertUserTraffic(t, st, 150, 200)
}

// TestFlushKeepsPendingOnError 断言写库失败时增量不丢：下一轮仍然带着上一轮的字节。
func TestFlushKeepsPendingOnError(t *testing.T) {
	ctx := context.Background()
	failing := &failingStore{Store: testStore(t), fail: true}
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(10)

	m.Flush(ctx, failing)
	failing.fail = false
	m.Flush(ctx, failing)
	assertUserTraffic(t, failing.Store, 10, 0)
}

// failingStore 让流量写入失败，其余委托给内嵌的内存实现。
type failingStore struct {
	store.Store
	fail bool
}

func (s *failingStore) AddTrafficBatch(ctx context.Context, batch store.TrafficBatch) error {
	if s.fail {
		return errWrite
	}
	return s.Store.AddTrafficBatch(ctx, batch)
}

func (s *failingStore) AddUserTraffic(ctx context.Context, p store.Protocol, key string, up, down int64) error {
	if s.fail {
		return errWrite
	}
	return s.Store.AddUserTraffic(ctx, p, key, up, down)
}

func (s *failingStore) AddClientTraffic(ctx context.Context, key string, up, down int64) error {
	if s.fail {
		return errWrite
	}
	return s.Store.AddClientTraffic(ctx, key, up, down)
}

func (s *failingStore) AddPortMappingTraffic(ctx context.Context, id uint64, up, down int64) error {
	if s.fail {
		return errWrite
	}
	return s.Store.AddPortMappingTraffic(ctx, id, up, down)
}

func TestPortMappingTrafficFlush(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	for _, port := range []int{7000, 7001} {
		if err := st.SetPortMapping(ctx, store.PortMapping{Client: "edge-1", RemotePort: port,
			LocalHost: "127.0.0.1", LocalPort: 80, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListPortMappings(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("mappings = %v, err = %v", rows, err)
	}
	a, b := rows[0].ID, rows[1].ID
	m := NewMeter(nil)
	stats := m.PortMappingStats(a)
	if stats != m.PortMappingStats(a) || stats == m.PortMappingStats(b) {
		t.Fatal("mapping counters must be shared by ID and isolated between IDs")
	}
	stats.UpWrite.Add(100)
	stats.DownWrite.Add(200)
	m.PortMappingStats(b).DownWrite.Add(30)
	failing := &failingStore{Store: st, fail: true}
	m.Flush(ctx, failing)
	assertMappingTraffic(t, st, a, 0, 0)
	assertMappingTraffic(t, st, b, 0, 0)
	stats.UpWrite.Add(10)
	failing.fail = false
	m.Flush(ctx, failing)
	m.Flush(ctx, failing)
	assertMappingTraffic(t, st, a, 110, 200)
	assertMappingTraffic(t, st, b, 0, 30)

	m = NewMeter(nil)
	m.PortMappingStats(a).DownWrite.Add(5)
	m.Flush(ctx, st)
	assertMappingTraffic(t, st, a, 110, 205)
	if deleted, err := st.DeletePortMapping(ctx, a); err != nil || !deleted {
		t.Fatalf("delete mapping = %v, %v", deleted, err)
	}
	m.PortMappingStats(a).UpWrite.Add(15)
	m.Flush(ctx, st)
	if _, ok, err := st.GetPortMapping(ctx, a); err != nil || ok {
		t.Fatalf("deleted mapping recreated: ok=%v err=%v", ok, err)
	}
	assertMappingTraffic(t, st, b, 0, 30)
}

func assertMappingTraffic(t *testing.T, st store.Store, id uint64, up, down int64) {
	t.Helper()
	got, ok, err := st.GetPortMapping(context.Background(), id)
	if err != nil || !ok || got.Up != up || got.Down != down {
		t.Fatalf("mapping %d traffic = %d/%d, ok=%v err=%v; want %d/%d", id, got.Up, got.Down, ok, err, up, down)
	}
}

var errWrite = errors.New("write failed")

func assertUserTraffic(t *testing.T, st store.Store, up, down int64) {
	t.Helper()
	rows, err := st.ListUserTraffic(context.Background())
	if err != nil {
		t.Fatalf("list user traffic: %v", err)
	}
	if len(rows) != 1 || rows[0].Up != up || rows[0].Down != down {
		t.Fatalf("user traffic = %v, want one row %d/%d", rows, up, down)
	}
}

func assertClientTraffic(t *testing.T, st store.Store, up, down int64) {
	t.Helper()
	rows, err := st.ListClientTraffic(context.Background())
	if err != nil {
		t.Fatalf("list client traffic: %v", err)
	}
	if len(rows) != 1 || rows[0].Up != up || rows[0].Down != down {
		t.Fatalf("client traffic = %v, want one row %d/%d", rows, up, down)
	}
}

// countingStore 统计走了哪条落库路径。
type countingStore struct {
	store.Store
	batches int
	rows    int
}

func (s *countingStore) AddTrafficBatch(ctx context.Context, batch store.TrafficBatch) error {
	s.batches++
	return s.Store.AddTrafficBatch(ctx, batch)
}

func (s *countingStore) AddUserTraffic(ctx context.Context, p store.Protocol, key string, up, down int64) error {
	s.rows++
	return s.Store.AddUserTraffic(ctx, p, key, up, down)
}

func (s *countingStore) AddClientTraffic(ctx context.Context, key string, up, down int64) error {
	s.rows++
	return s.Store.AddClientTraffic(ctx, key, up, down)
}

// TestFlushPrefersBatch 断言支持整批写入的存储走单事务，不再逐条提交。
func TestFlushPrefersBatch(t *testing.T) {
	ctx := context.Background()
	st := &countingStore{Store: testStore(t)}
	m := NewMeter(nil)
	m.RestoreUser(store.ProtocolVNV1, "alice")
	m.RestoreUser(store.ProtocolVMess, "uuid-1")
	m.UserStats(store.ProtocolVNV1, "alice").UpWrite.Add(10)
	m.UserStats(store.ProtocolVMess, "uuid-1").UpWrite.Add(20)
	m.ClientStats("edge-1").DownWrite.Add(30)

	if err := m.Flush(ctx, st); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if st.batches != 1 || st.rows != 0 {
		t.Fatalf("batches=%d rows=%d, want 1/0", st.batches, st.rows)
	}
	if rows, _ := st.ListUserTraffic(ctx); len(rows) != 2 {
		t.Fatalf("user traffic rows = %v, want 2", rows)
	}
}
