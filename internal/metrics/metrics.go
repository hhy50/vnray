// Package metrics 提供进程内性能指标原语：计数器、读数与固定桶直方图，
// 以及按名注册的默认注册表。热路径方法全部是 atomic 操作，无锁、无分配。
//
// 默认注册表是进程级单例，与 internal/logging 的全局 logger 同属特许例外：
// 注册发生在各包 init/装配期，名称集合静态有限，运行期只做原子读写。
package metrics

import (
	"math"
	"runtime"
	"runtime/pprof"
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
	mu           sync.RWMutex
	seriesByName = map[string]*series{}
	// enabled 是采集总开关：关闭后所有记录方法变为 no-op，快照接口也应停服。
	enabled atomic.Bool
)

func init() { enabled.Store(true) }

// SetEnabled 全局开关指标采集，立即生效；已累计的读数保留不清零。
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled 返回采集开关的当前状态。
func Enabled() bool { return enabled.Load() }

// register 创建或复用同名序列；kind/bounds 不一致视为编程错误，直接 panic
// （注册发生在初始化期，错误在启动时即暴露）。
func register(name, kind string, bounds []float64) *series {
	mu.Lock()
	defer mu.Unlock()
	if s, ok := seriesByName[name]; ok {
		if s.kind != kind {
			panic("metrics: " + name + " registered as " + s.kind + " and " + kind)
		}
		return s
	}
	s := &series{name: name, kind: kind, bounds: bounds}
	s.buckets = make([]atomic.Int64, len(bounds)+1)
	seriesByName[name] = s
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
	if !enabled.Load() {
		return
	}
	c.s.value.Add(1)
}

// Add 累加 n；负数按 0 处理，计数器不允许回退。
func (c *Counter) Add(n int64) {
	if n <= 0 || !enabled.Load() {
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

// Add 增减读数（传负数即减少）。
func (g *Gauge) Add(delta int64) {
	if !enabled.Load() {
		return
	}
	g.s.value.Add(delta)
}

// Set 覆盖读数。
func (g *Gauge) Set(v int64) {
	if !enabled.Load() {
		return
	}
	g.s.value.Store(v)
}

// Value 返回当前读数。
func (g *Gauge) Value() int64 { return g.s.value.Load() }

// Histogram 是固定桶直方图：Observe 只做一次线性扫桶与一次 CAS 求和。
type Histogram struct{ s *series }

// NewHistogram 注册并返回一个直方图；bounds 须升序，最后补一个 +Inf 桶。
func NewHistogram(name string, bounds []float64) *Histogram {
	return &Histogram{s: register(name, TypeHistogram, bounds)}
}

// Observe 记录一次观测值。
func (h *Histogram) Observe(v float64) {
	s := h.s
	if !enabled.Load() {
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
	mu.RLock()
	names := make([]string, 0, len(seriesByName))
	for name := range seriesByName {
		names = append(names, name)
	}
	mu.RUnlock()
	sort.Strings(names)

	out := make([]Series, 0, len(names))
	for _, name := range names {
		mu.RLock()
		s := seriesByName[name]
		mu.RUnlock()
		out = append(out, snapshotSeries(s))
	}
	return out
}

func snapshotSeries(s *series) Series {
	out := Series{Name: s.name, Type: s.kind}
	switch s.kind {
	case TypeHistogram:
		out.Count = s.observed.Load()
		out.Sum = math.Float64frombits(s.sumBits.Load())
		out.Buckets = make([]Bucket, 0, len(s.buckets))
		for i := range s.buckets {
			var upper float64 = math.Inf(1)
			if i < len(s.bounds) {
				upper = s.bounds[i]
			}
			out.Buckets = append(out.Buckets, Bucket{Upper: upper, Count: s.buckets[i].Load()})
		}
	default:
		out.Value = s.value.Load()
	}
	return out
}

// RuntimeInfo 是进程 runtime 在快照时刻的读数。CPU 占用没有跨平台的无依赖
// 取法，这里用 GC CPU 分数近似；精确进程 CPU 需外部采集器。
type RuntimeInfo struct {
	Goroutines    int     `json:"goroutines"`
	Gomaxprocs    int     `json:"gomaxprocs"`
	Threads       int     `json:"threads"`
	HeapAlloc     uint64  `json:"heap_alloc"`
	HeapObjects   uint64  `json:"heap_objects"`
	HeapSys       uint64  `json:"heap_sys"`
	Sys           uint64  `json:"sys_mem"`
	NextGC        uint64  `json:"next_gc"`
	NumGC         uint32  `json:"gc_count"`
	GCPauseLastNs uint64  `json:"gc_pause_last_ns"`
	GCCPUFraction float64 `json:"gc_cpu_fraction"`
}

// RuntimeSnapshot 读取 runtime 指标。ReadMemStats 有短暂 STW，只允许暴露层调用。
func RuntimeSnapshot() RuntimeInfo {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	info := RuntimeInfo{
		Goroutines:    runtime.NumGoroutine(),
		Gomaxprocs:    runtime.GOMAXPROCS(0),
		Threads:       threadCount(),
		HeapAlloc:     ms.HeapAlloc,
		HeapObjects:   ms.HeapObjects,
		HeapSys:       ms.HeapSys,
		Sys:           ms.Sys,
		NextGC:        ms.NextGC,
		NumGC:         ms.NumGC,
		GCCPUFraction: ms.GCCPUFraction,
	}
	// PauseNs 是环形数组，取最近一次暂停。
	if n := ms.NumGC; n > 0 {
		info.GCPauseLastNs = ms.PauseNs[(n-1)%uint32(len(ms.PauseNs))]
	}
	return info
}

// threadCount 取 OS 线程数；profile 实例由 runtime 维护，Count 无锁且廉价。
func threadCount() int {
	if p := pprof.Lookup("threadcreate"); p != nil {
		return p.Count()
	}
	return 0
}
