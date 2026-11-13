package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/nakabonne/tstorage"

	"github.com/hhy50/vnray/internal/logging"
)

// 分区 10 分钟落盘一次；WAL 已关闭，进程崩溃会丢失未刷盘的尾部（最多两个分区）。
const historyPartitionDuration = 10 * time.Minute

// History 管理独立的 tstorage 指标目录及串行读写生命周期。
type History struct {
	gate      chan struct{}
	store     tstorage.Storage
	dir       string
	retention time.Duration
	now       func() time.Time
	open      func(string, time.Duration) (tstorage.Storage, error)
	last      []tstorage.Row
	lastAt    int64
	closed    bool
	closeErr  error
}

// NewHistory 打开监控目录；调用方必须独占目录并在停止采样后关闭。
func NewHistory(dir string, retention time.Duration) (*History, error) {
	if dir == "" {
		return nil, errors.New("monitoring: history directory is required")
	}
	return newHistory(dir, retention, time.Now)
}

func newHistory(dir string, retention time.Duration, now func() time.Time) (*History, error) {
	if retention <= 0 {
		return nil, errors.New("monitoring: positive retention is required")
	}
	st, err := openHistory(dir, retention)
	if err != nil {
		return nil, err
	}
	return &History{gate: make(chan struct{}, 1), store: st, dir: dir, retention: retention, now: now, open: openHistory}, nil
}

func openHistory(dir string, retention time.Duration) (tstorage.Storage, error) {
	st, err := tstorage.NewStorage(
		tstorage.WithDataPath(dir),
		tstorage.WithPartitionDuration(historyPartitionDuration),
		tstorage.WithRetention(retention),
		tstorage.WithTimestampPrecision(tstorage.Nanoseconds),
		tstorage.WithWALBufferedSize(-1),
		tstorage.WithWriteTimeout(storageTimeout),
		tstorage.WithLogger(historyLogger{}),
	)
	if err != nil {
		return nil, fmt.Errorf("monitoring: open history: %w", err)
	}
	return st, nil
}

type historyLogger struct{}

func (historyLogger) Printf(format string, args ...any) {
	logging.Warnf("monitoring: tstorage: "+format, args...)
}

func (h *History) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *History) available() error {
	if h.closed {
		return errors.New("monitoring: history storage is closed")
	}
	if h.store == nil {
		return errors.New("monitoring: history storage is unavailable")
	}
	return nil
}

// Save 批量写入变化指标，完成标记位于批次末尾，成功后推进去重基线。
func (h *History) Save(ctx context.Context, sample Snapshot) error {
	if err := h.lock(ctx); err != nil {
		return err
	}
	defer func() { <-h.gate }()
	if err := h.available(); err != nil {
		return err
	}
	rows, err := snapshotRows(sample)
	if err != nil {
		return err
	}
	if sameRows(rows, h.last) {
		return nil
	}
	at := sample.SampledAt.UnixNano()
	if at <= h.lastAt || at == math.MaxInt64 {
		return errors.New("monitoring: sample timestamp must be positive and increase")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.store.InsertRows(rows); err != nil {
		return fmt.Errorf("monitoring: insert history: %w", err)
	}
	h.last, h.lastAt = rows, at
	return nil
}

// Read 按完成标记选取最多 120 个时间点，再重建主机及指定用户的指标。
func (h *History) Read(ctx context.Context, since, until time.Time, limit int, ids []uint64) ([]Snapshot, error) {
	if limit < 2 || limit > 120 {
		return nil, errors.New("monitoring: history limit must be 2..120")
	}
	if err := h.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-h.gate }()
	if err := h.available(); err != nil {
		return nil, err
	}
	start, end := max(since.UnixNano(), h.now().Add(-h.retention).UnixNano()), until.UnixNano()
	if start > end || start == math.MaxInt64 {
		return []Snapshot{}, nil
	}
	if end < math.MaxInt64 {
		end++
	}
	points, err := h.selectPoints(ctx, sampleMetric, nil, start, end)
	if err != nil {
		return nil, err
	}
	times := make([]int64, 0, len(points))
	for _, p := range points {
		if p.Value != 1 {
			return nil, errors.New("monitoring: invalid sample marker")
		}
		times = append(times, p.Timestamp)
	}
	slices.Sort(times)
	times = slices.Compact(times)
	if len(times) > limit {
		selected := make([]int64, limit)
		for i := range selected {
			selected[i] = times[i*(len(times)-1)/(limit-1)]
		}
		times = selected
	}
	result := make([]Snapshot, 0, len(times))
	for len(times) > 0 {
		n := 1
		for n < len(times) && times[n]-times[0] < int64(historyPartitionDuration) {
			n++
		}
		group, err := h.readSnapshots(ctx, times[:n], ids)
		if err != nil {
			return nil, err
		}
		result = append(result, group...)
		times = times[n:]
	}
	return result, nil
}

func (h *History) selectPoints(ctx context.Context, metric string, labels []tstorage.Label, start, end int64) ([]*tstorage.DataPoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 上游 Select 不接受 context；只能在两次引擎调用之间响应取消。
	points, err := h.store.Select(metric, labels, start, end)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, tstorage.ErrNoDataPoints) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("monitoring: read %s: %w", metric, err)
	}
	return points, nil
}

// Configure 更新查询保留期，变化时关闭并重开引擎以应用磁盘保留策略。
func (h *History) Configure(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return errors.New("monitoring: invalid retention")
	}
	if err := h.lock(ctx); err != nil {
		return err
	}
	defer func() { <-h.gate }()
	if h.closed {
		return errors.New("monitoring: history storage is closed")
	}
	if h.closeErr != nil {
		return h.closeErr
	}
	if h.retention == retention && h.store != nil {
		return nil
	}
	h.retention = retention
	if h.store != nil {
		h.closeErr = h.store.Close()
		h.store = nil
		if h.closeErr != nil {
			return h.closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := h.open(h.dir, retention)
	if err != nil {
		return err
	}
	h.store, h.last = st, nil
	return nil
}

// Close 等待在途操作并关闭引擎，重复调用返回同一结果。
func (h *History) Close() error {
	h.gate <- struct{}{}
	defer func() { <-h.gate }()
	if !h.closed {
		h.closed = true
		if h.store != nil {
			h.closeErr = h.store.Close()
			h.store = nil
		}
		h.last = nil
	}
	return h.closeErr
}
