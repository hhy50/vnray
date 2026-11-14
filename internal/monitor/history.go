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

// WAL 已关闭，崩溃会丢失尚未刷盘的内存尾部；分区由后续写入推动轮转。
const historyPartitionDuration = 10 * time.Minute

const historyTimestampResolution = 100 * time.Millisecond

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
		tstorage.WithTimestampPrecision(tstorage.Milliseconds),
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
	at := sample.SampledAt.Truncate(historyTimestampResolution).UnixMilli()
	if at <= h.lastAt || at == math.MaxInt64 {
		return errors.New("monitoring: sample timestamp must be positive and increase at 100ms resolution")
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
	start, end := max(ceilMillisecond(since), ceilMillisecond(h.now().Add(-h.retention))), until.UnixMilli()
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
		times = selectEvenly(times, limit)
	}
	result := make([]Snapshot, 0, len(times))
	for len(times) > 0 {
		n := 1
		for n < len(times) && times[n]-times[0] < historyPartitionDuration.Milliseconds() {
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

// selectEvenly 按时间均匀取网格点就近选取；去重跳写留下的空窗会让多个网格命中
// 同一点，去重后输出可能少于 limit，但时间分布均匀且保持有序不重复。
func selectEvenly(times []int64, limit int) []int64 {
	start := times[0]
	step := (times[len(times)-1] - start) / int64(limit-1)
	selected := make([]int64, 0, limit)
	j, last := 0, -1
	for i := range limit {
		target := start + int64(i)*step
		for j+1 < len(times) && times[j+1] <= target {
			j++
		}
		nearest := j
		if j+1 < len(times) && times[j+1]-target < target-times[j] {
			nearest = j + 1
		}
		if nearest < last {
			nearest = last
		}
		if nearest != last {
			selected = append(selected, times[nearest])
			last = nearest
		}
	}
	return selected
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
	// 上游内存分区在 end 等于末点时可能返回末点。
	for len(points) > 0 && points[len(points)-1].Timestamp >= end {
		points = points[:len(points)-1]
	}
	return points, nil
}

// 下限向上取整，避免查询窗口或保留期边界带入已经过期的样本。
func ceilMillisecond(at time.Time) int64 {
	value := at.UnixMilli()
	if at.Nanosecond()%int(time.Millisecond) != 0 {
		value++
	}
	return value
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
