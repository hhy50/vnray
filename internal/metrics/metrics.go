// Package metrics 提供进程内性能指标原语：计数器、读数与固定桶直方图，
// 以及按名注册的默认注册表。热路径方法全部是 atomic 操作，无锁、无分配。
//
// 默认注册表是进程级单例，与 internal/logging 的全局 logger 同属特许例外：
// 注册发生在各包 init/装配期，名称集合静态有限，运行期只做原子读写。
package metrics

import (
	"encoding/json"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
)

// 指标类型常量，随快照输出。
const (
	TypeCounter   = "counter"
	TypeGauge     = "gauge"
	TypeHistogram = "histogram"
)

// DurationBounds 是秒级时延直方图的默认桶边界：覆盖微秒级本地转发到秒级出口拨号。
var DurationBounds = []float64{
	0.000005, 0.000025, 0.0001, 0.0005, 0.001, 0.005, 0.025, 0.1, 0.25, 1, 5,
}

// series 是一条命名序列的存储；counter/gauge 用 value，histogram 另用桶计数。
type series struct {
	name     string
	kind     string
	value    atomic.Int64
	observed atomic.Int64
	sumBits  atomic.Uint64
	bounds   []float64
	buckets  []atomic.Int64
}

var (
	mu               sync.Mutex
	seriesByName     = map[string]*series{}
	registeredSeries atomic.Pointer[[]*series]
	// 状态量必须跨开关维护配对增减；开关只暂停事件计数与后台采样。
	collectionState atomic.Uint64
)

func init() { collectionState.Store(1) }

// SetEnabled 暂停事件采集与快照服务；状态量继续维护，累计值不清零。
func SetEnabled(on bool) {
	for {
		state := collectionState.Load()
		if (state&1 != 0) == on {
			return
		}
		if collectionState.CompareAndSwap(state, state+1) {
			return
		}
	}
}

// Enabled 返回采集开关的当前状态。
func Enabled() bool { return collectionState.Load()&1 != 0 }

// register 创建或复用同名序列；kind/bounds 不一致视为编程错误，直接 panic
// （注册发生在初始化期，错误在启动时即暴露）。
func register(name, kind string, bounds []float64) *series {
	mu.Lock()
	defer mu.Unlock()
	if s, ok := seriesByName[name]; ok {
		if s.kind != kind || !slices.Equal(s.bounds, bounds) {
			panic("metrics: " + name + " registered as " + s.kind + " and " + kind)
		}
		return s
	}
	s := &series{name: name, kind: kind, bounds: bounds}
	s.buckets = make([]atomic.Int64, len(bounds)+1)
	seriesByName[name] = s
	registered := make([]*series, 0, len(seriesByName))
	for _, entry := range seriesByName {
		registered = append(registered, entry)
	}
	sort.Slice(registered, func(i, j int) bool { return registered[i].name < registered[j].name })
	registeredSeries.Store(&registered)
	return s
}

// Counter 是单调递增计数器。
type Counter struct{ s *series }

// NewCounter 注册并返回一个计数器；同名重复注册返回同一实例。
func NewCounter(name string) *Counter {
	return &Counter{s: register(name, TypeCounter, nil)}
}

// Inc 计数加一。
func (c *Counter) Inc() {
	if !Enabled() {
		return
	}
	c.s.value.Add(1)
}

// Add 累加 n；负数按 0 处理，计数器不允许回退。
func (c *Counter) Add(n int64) {
	if n <= 0 || !Enabled() {
		return
	}
	c.s.value.Add(n)
}

// Value 返回当前读数。
func (c *Counter) Value() int64 { return c.s.value.Load() }

// Gauge 是可增可减的读数。
type Gauge struct{ s *series }

// NewGauge 注册并返回一个读数；同名重复注册返回同一实例。
func NewGauge(name string) *Gauge {
	return &Gauge{s: register(name, TypeGauge, nil)}
}

// GaugeFor 按需注册动态名称的读数并返回同一实例；用于名称在运行期才确定、
// 但集合有限的场景（如按协程名聚合）。
func GaugeFor(name string) *Gauge {
	return &Gauge{s: register(name, TypeGauge, nil)}
}

// CounterFor 按需注册动态名称的计数器，语义同 GaugeFor。
func CounterFor(name string) *Counter {
	return &Counter{s: register(name, TypeCounter, nil)}
}

// Add 增减状态量，即使暂停采集也保持生命周期配对。
func (g *Gauge) Add(delta int64) { g.s.value.Add(delta) }

// Set 覆盖读数。
func (g *Gauge) Set(v int64) { g.s.value.Store(v) }

// Value 返回当前读数。
func (g *Gauge) Value() int64 { return g.s.value.Load() }

// Histogram 是固定桶直方图：Observe 只做一次线性扫桶与一次 CAS 求和。
type Histogram struct{ s *series }

// NewHistogram 注册并返回一个直方图；bounds 须升序，最后补一个 +Inf 桶。
func NewHistogram(name string, bounds []float64) *Histogram {
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 || (i > 0 && b <= bounds[i-1]) {
			panic("metrics: invalid histogram bounds")
		}
	}
	bounds = append([]float64(nil), bounds...)
	return &Histogram{s: register(name, TypeHistogram, bounds)}
}

// Observe 记录一次观测值。
func (h *Histogram) Observe(v float64) {
	s := h.s
	if !Enabled() || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	s.observed.Add(1)
	for i, b := range s.bounds {
		if v <= b {
			s.buckets[i].Add(1)
			h.addSum(v)
			return
		}
	}
	s.buckets[len(s.buckets)-1].Add(1)
	h.addSum(v)
}

// addSum 用 CAS 更新 float64 总和。
func (h *Histogram) addSum(v float64) {
	for {
		old := h.s.sumBits.Load()
		next := math.Float64frombits(old) + v
		if h.s.sumBits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// Count 返回观测次数。
func (h *Histogram) Count() int64 { return h.s.observed.Load() }

// Sum 返回观测值总和。
func (h *Histogram) Sum() float64 { return math.Float64frombits(h.s.sumBits.Load()) }

// Bucket 是直方图快照里的一只桶：Upper 为上界，最后一只为 +Inf。
type Bucket struct {
	Upper float64 `json:"le"`
	Count int64   `json:"count"`
}

// Series 是一条指标在快照时刻的读数。
type Series struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Value   int64    `json:"value,omitempty"`
	Count   int64    `json:"count,omitempty"`
	Sum     float64  `json:"sum,omitempty"`
	Buckets []Bucket `json:"buckets,omitempty"`
}

// Snapshot 输出全部注册序列，按名称排序；只供暴露层低频调用。
func Snapshot() []Series {
	registered := registeredSeries.Load()
	if registered == nil {
		return []Series{}
	}
	out := make([]Series, 0, len(*registered))
	for _, s := range *registered {
		out = append(out, snapshotSeries(s))
	}
	return out
}

func snapshotSeries(s *series) Series {
	out := Series{Name: s.name, Type: s.kind}
	switch s.kind {
	case TypeHistogram:
		out.Sum = math.Float64frombits(s.sumBits.Load())
		out.Buckets = make([]Bucket, 0, len(s.buckets))
		for i := range s.buckets {
			var upper float64 = math.Inf(1)
			if i < len(s.bounds) {
				upper = s.bounds[i]
			}
			count := s.buckets[i].Load()
			out.Count += count
			out.Buckets = append(out.Buckets, Bucket{Upper: upper, Count: count})
		}
	default:
		out.Value = s.value.Load()
	}
	return out
}

// MarshalJSON 用 null 表示无穷上界，避免 encoding/json 拒绝整个快照。
func (b Bucket) MarshalJSON() ([]byte, error) {
	var upper *float64
	if !math.IsInf(b.Upper, 0) {
		upper = &b.Upper
	}
	return json.Marshal(struct {
		Upper *float64 `json:"le"`
		Count int64    `json:"count"`
	}{upper, b.Count})
}
