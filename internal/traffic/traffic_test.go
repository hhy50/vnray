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
	failing := &failingStore{Store: testStore(t)}
	m := NewMeter(nil)
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
