package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/nakabonne/tstorage"
)

const (
	sampleMetric = "vnray.monitor.sample"
	hostPrefix   = "vnray.monitor.host."
	userPrefix   = "vnray.monitor.user."
)

type seriesCodec struct {
	value func(string, float64) (float64, error)
	err   error
}

func (c *seriesCodec) number(name string, value float64) float64 {
	if c.err != nil {
		return 0
	}
	v, err := c.value(name, value)
	if err != nil {
		c.err = err
	} else if math.IsInf(v, 0) {
		c.err = fmt.Errorf("monitoring: non-finite metric %s", name)
	}
	return v
}

func (c *seriesCodec) floating(name string, value *float64) *float64 {
	v := math.NaN()
	if value != nil {
		if math.IsNaN(*value) || math.IsInf(*value, 0) {
			c.err = fmt.Errorf("monitoring: non-finite metric %s", name)
			return nil
		}
		v = *value
	}
	v = c.number(name, v)
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// 64 位整数拆为可精确表示的两个 uint32；同时为 NaN 表示缺失。
func (c *seriesCodec) optionalUint(name string, value *uint64) *uint64 {
	hi, lo := math.NaN(), math.NaN()
	if value != nil {
		hi, lo = float64(*value>>32), float64(uint32(*value))
	}
	hi, lo = c.number(name+"_hi", hi), c.number(name+"_lo", lo)
	if c.err != nil {
		return nil
	}
	if math.IsNaN(hi) && math.IsNaN(lo) {
		return nil
	}
	valid := func(v float64) bool { return v >= 0 && v <= math.MaxUint32 && math.Trunc(v) == v }
	if !valid(hi) || !valid(lo) {
		c.err = fmt.Errorf("monitoring: invalid integer metric %s", name)
		return nil
	}
	v := uint64(hi)<<32 | uint64(lo)
	return &v
}

func (c *seriesCodec) unsigned(name string, value uint64) uint64 {
	v := c.optionalUint(name, &value)
	if v == nil {
		if c.err == nil {
			c.err = fmt.Errorf("monitoring: missing integer metric %s", name)
		}
		return 0
	}
	return *v
}

func (c *seriesCodec) integer(name string, value int64) int64 {
	return int64(c.unsigned(name, uint64(value)))
}

func (c *seriesCodec) smallInt(name string, value int) int {
	v := c.number(name, float64(value))
	if v < 0 || v >= float64(math.MaxInt) || math.IsNaN(v) || math.Trunc(v) != v {
		c.err = fmt.Errorf("monitoring: invalid integer metric %s", name)
		return 0
	}
	return int(v)
}

func (c *seriesCodec) host(s *Snapshot) {
	s.Server.CPUPercent = c.floating("cpu_percent", s.Server.CPUPercent)
	s.Server.MemoryTotal = c.optionalUint("memory_total", s.Server.MemoryTotal)
	s.Server.MemoryUsed = c.optionalUint("memory_used", s.Server.MemoryUsed)
	s.Server.ReceiveBytesPerSecond = c.floating("receive_bytes_per_second", s.Server.ReceiveBytesPerSecond)
	s.Server.TransmitBytesPerSecond = c.floating("transmit_bytes_per_second", s.Server.TransmitBytesPerSecond)
	s.Server.Interfaces = c.smallInt("interfaces", s.Server.Interfaces)
	s.Process.CPUPercent = c.floating("process_cpu_percent", s.Process.CPUPercent)
	s.Process.ResidentBytes = c.optionalUint("resident_bytes", s.Process.ResidentBytes)
	s.Process.Goroutines = c.unsigned("goroutines", s.Process.Goroutines)
	s.Process.Gomaxprocs = c.smallInt("gomaxprocs", s.Process.Gomaxprocs)
	s.Process.HeapAlloc = c.unsigned("heap_alloc", s.Process.HeapAlloc)
	s.Process.HeapObjects = c.unsigned("heap_objects", s.Process.HeapObjects)
	s.Process.GoMemory = c.unsigned("go_memory", s.Process.GoMemory)
	s.Process.NextGC = c.unsigned("next_gc", s.Process.NextGC)
	s.Process.GCCount = c.unsigned("gc_count", s.Process.GCCount)
	s.Process.GCPauseP99Seconds = c.floating("gc_pause_p99_seconds", s.Process.GCPauseP99Seconds)
	s.Process.GCCPUPercent = c.floating("gc_cpu_percent", s.Process.GCCPUPercent)
}

func (c *seriesCodec) user(u *User) {
	u.TCP = c.integer("tcp", u.TCP)
	u.Streams = c.integer("streams", u.Streams)
	u.Requests = c.integer("requests", u.Requests)
	u.Up = c.integer("up", u.Up)
	u.Down = c.integer("down", u.Down)
	u.UpRate = c.floating("up_rate", u.UpRate)
	u.DownRate = c.floating("down_rate", u.DownRate)
}

func userLabels(id uint64) []tstorage.Label {
	return []tstorage.Label{{Name: "user_id", Value: strconv.FormatUint(id, 10)}}
}

func snapshotRows(s Snapshot) ([]tstorage.Row, error) {
	rows := make([]tstorage.Row, 0, 27+12*len(s.Users))
	writer := func(prefix string, labels []tstorage.Label) *seriesCodec {
		return &seriesCodec{value: func(name string, value float64) (float64, error) {
			rows = append(rows, tstorage.Row{Metric: prefix + name, Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: s.SampledAt.UnixNano(), Value: value}})
			return value, nil
		}}
	}
	c := writer(hostPrefix, nil)
	c.host(&s)
	if c.err != nil {
		return nil, c.err
	}
	users := slices.Clone(s.Users)
	slices.SortFunc(users, func(a, b User) int { return cmp.Compare(a.ID, b.ID) })
	for i, u := range users {
		if i > 0 && u.ID == users[i-1].ID {
			return nil, errors.New("monitoring: duplicate user ID")
		}
		c := writer(userPrefix, userLabels(u.ID))
		c.user(&u)
		if c.err != nil {
			return nil, c.err
		}
	}
	rows = append(rows, tstorage.Row{Metric: sampleMetric, DataPoint: tstorage.DataPoint{Timestamp: s.SampledAt.UnixNano(), Value: 1}})
	return rows, nil
}

func sameRows(a, b []tstorage.Row) bool {
	return slices.EqualFunc(a, b, func(a, b tstorage.Row) bool {
		return a.Metric == b.Metric && slices.Equal(a.Labels, b.Labels) && math.Float64bits(a.Value) == math.Float64bits(b.Value)
	})
}

func (h *History) readSnapshots(ctx context.Context, times []int64, ids []uint64) ([]Snapshot, error) {
	selected := make(map[int64]bool, len(times))
	for _, at := range times {
		selected[at] = true
	}
	type seriesKey struct{ metric, user string }
	cache := make(map[seriesKey]map[int64]float64)
	lookup := func(metric string, labels []tstorage.Label) (map[int64]float64, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := seriesKey{metric: metric}
		if len(labels) > 0 {
			key.user = labels[0].Value
		}
		if values, ok := cache[key]; ok {
			return values, nil
		}
		points, err := h.selectPoints(ctx, metric, labels, times[0], times[len(times)-1]+1)
		if err != nil {
			return nil, err
		}
		values := make(map[int64]float64, len(times))
		for _, point := range points {
			if selected[point.Timestamp] {
				values[point.Timestamp] = point.Value
			}
		}
		cache[key] = values
		return values, nil
	}
	reader := func(at int64, prefix string, labels []tstorage.Label) *seriesCodec {
		return &seriesCodec{value: func(name string, _ float64) (float64, error) {
			values, err := lookup(prefix+name, labels)
			if err != nil {
				return 0, err
			}
			value, ok := values[at]
			if !ok {
				return 0, fmt.Errorf("monitoring: incomplete sample %d: %s", at, prefix+name)
			}
			return value, nil
		}}
	}
	users := make([]uint64, 0, len(ids))
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			users = append(users, id)
			seen[id] = true
		}
	}
	result := make([]Snapshot, 0, len(times))
	for _, at := range times {
		s := Snapshot{SampledAt: time.Unix(0, at).UTC(), Users: []User{}}
		c := reader(at, hostPrefix, nil)
		c.host(&s)
		if c.err != nil {
			return nil, c.err
		}
		for _, id := range users {
			labels := userLabels(id)
			values, err := lookup(userPrefix+"tcp_hi", labels)
			if err != nil {
				return nil, err
			}
			if _, ok := values[at]; !ok {
				continue
			}
			u := User{ID: id}
			c := reader(at, userPrefix, labels)
			c.user(&u)
			if c.err != nil {
				return nil, c.err
			}
			s.Users = append(s.Users, u)
		}
		result = append(result, s)
	}
	return result, nil
}
