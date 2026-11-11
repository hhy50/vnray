package metrics

import (
	"math"
	"runtime"
	runtimemetrics "runtime/metrics"
	"time"
)

// ServerInfo 是操作系统可见的主机资源，不代表容器配额；nil 表示当前不可用。
type ServerInfo struct {
	CPUPercent             *float64 `json:"cpu_percent"`
	MemoryTotal            *uint64  `json:"memory_total"`
	MemoryUsed             *uint64  `json:"memory_used"`
	ReceiveBytesPerSecond  *float64 `json:"receive_bytes_per_second"`
	TransmitBytesPerSecond *float64 `json:"transmit_bytes_per_second"`
	Interfaces             int      `json:"interfaces"`
}

// ProcessInfo 区分进程驻留内存与 Go 内存；CPU 按单核 100% 计，可超过 100%。
type ProcessInfo struct {
	CPUPercent        *float64 `json:"cpu_percent"`
	ResidentBytes     *uint64  `json:"resident_bytes"`
	Goroutines        uint64   `json:"goroutines"`
	Gomaxprocs        int      `json:"gomaxprocs"`
	HeapAlloc         uint64   `json:"heap_alloc"`
	HeapObjects       uint64   `json:"heap_objects"`
	GoMemory          uint64   `json:"go_memory"`
	NextGC            uint64   `json:"next_gc"`
	GCCount           uint64   `json:"gc_count"`
	GCPauseP99Seconds *float64 `json:"gc_pause_p99_seconds"`
	GCCPUPercent      *float64 `json:"gc_cpu_percent"`
}

// Sample 是一次后台采样；发布后只读，HTTP 不执行系统调用或 runtime 采样。
type Sample struct {
	epoch     uint64
	SampledAt time.Time   `json:"sampled_at"`
	Server    ServerInfo  `json:"server"`
	Process   ProcessInfo `json:"process"`
	Metrics   []Series    `json:"metrics"`
}

// Current 判断快照是否仍属于当前采集周期。
func (s Sample) Current() bool { return s.epoch == collectionState.Load() }

const (
	runtimeGoroutines = iota
	runtimeHeapObjects
	runtimeMemoryTotal
	runtimeHeapReleased
	runtimeHeapGoal
	runtimeGCCycles
	runtimeGCPauses
	runtimeGCCPU
	runtimeTotalCPU
	runtimeHeapBytes
)

type networkCounter struct{ receive, transmit uint64 }
type systemReading struct {
	cpuTotal, cpuIdle                 uint64
	cpuValid                          bool
	processCPU                        *float64
	resident, memoryTotal, memoryUsed *uint64
	network                           map[string]networkCounter
}

// Sampler 由唯一采样协程调用；复用 runtime 读数存储并维护速率基线。
type Sampler struct {
	previous        systemReading
	at              time.Time
	generation      uint64
	runtime         []runtimemetrics.Sample
	gcCPU, totalCPU float64
}

// NewSampler 创建不启动后台任务的采样器。
func NewSampler() *Sampler {
	names := []string{
		"/sched/goroutines:goroutines", "/gc/heap/objects:objects",
		"/memory/classes/total:bytes", "/memory/classes/heap/released:bytes",
		"/gc/heap/goal:bytes", "/gc/cycles/total:gc-cycles", "/sched/pauses/total/gc:seconds",
		"/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/total:cpu-seconds",
		"/memory/classes/heap/objects:bytes",
	}
	s := &Sampler{runtime: make([]runtimemetrics.Sample, len(names))}
	for i, name := range names {
		s.runtime[i].Name = name
	}
	return s
}

// Reset 丢弃速率基线，避免暂停采集期间的变化被挤进一个采样周期。
func (s *Sampler) Reset() { s.at = time.Time{}; s.previous = systemReading{} }

// Collect 读取本机资源；只能从后台调用，不能放在请求或转发路径。
func (s *Sampler) Collect() Sample {
	epoch := collectionState.Load()
	if s.generation != epoch {
		s.Reset()
		s.generation = epoch
	}
	reading := readSystem()
	now := time.Now()
	runtimemetrics.Read(s.runtime)
	out := Sample{epoch: epoch, SampledAt: now, Metrics: Snapshot()}
	out.Server = serverInfo(reading, s.previous, now.Sub(s.at).Seconds(), !s.at.IsZero())
	p := ProcessInfo{
		ResidentBytes: reading.resident,
		Goroutines:    s.runtime[runtimeGoroutines].Value.Uint64(), Gomaxprocs: runtime.GOMAXPROCS(0),
		HeapAlloc: s.runtime[runtimeHeapBytes].Value.Uint64(), HeapObjects: s.runtime[runtimeHeapObjects].Value.Uint64(),
		GoMemory: s.runtime[runtimeMemoryTotal].Value.Uint64() - s.runtime[runtimeHeapReleased].Value.Uint64(),
		NextGC:   s.runtime[runtimeHeapGoal].Value.Uint64(), GCCount: s.runtime[runtimeGCCycles].Value.Uint64(),
		GCPauseP99Seconds: histogramQuantile(s.runtime[runtimeGCPauses].Value.Float64Histogram(), 0.99),
	}
	gcCPU, totalCPU := s.runtime[runtimeGCCPU].Value.Float64(), s.runtime[runtimeTotalCPU].Value.Float64()
	if !s.at.IsZero() {
		if reading.processCPU != nil && s.previous.processCPU != nil {
			p.CPUPercent = rate(*reading.processCPU, *s.previous.processCPU, now.Sub(s.at).Seconds(), 100)
		}
		p.GCCPUPercent = rate(gcCPU, s.gcCPU, totalCPU-s.totalCPU, 100)
	}
	out.Process = p
	s.previous, s.at, s.gcCPU, s.totalCPU = reading, now, gcCPU, totalCPU
	return out
}

func serverInfo(current, previous systemReading, seconds float64, baseline bool) ServerInfo {
	s := ServerInfo{MemoryTotal: current.memoryTotal, MemoryUsed: current.memoryUsed, Interfaces: len(current.network)}
	if !baseline {
		return s
	}
	if current.cpuValid && previous.cpuValid && current.cpuTotal > previous.cpuTotal && current.cpuIdle >= previous.cpuIdle {
		total, idle := current.cpuTotal-previous.cpuTotal, current.cpuIdle-previous.cpuIdle
		if idle <= total {
			s.CPUPercent = pointer(100 * float64(total-idle) / float64(total))
		}
	}
	// 接口消失、新增或计数器重置时不做无符号减法，也不制造速率尖峰。
	if current.network != nil && previous.network != nil && seconds > 0 {
		var rx, tx uint64
		for name, c := range current.network {
			if p, ok := previous.network[name]; ok {
				if c.receive >= p.receive {
					rx += c.receive - p.receive
				}
				if c.transmit >= p.transmit {
					tx += c.transmit - p.transmit
				}
			}
		}
		s.ReceiveBytesPerSecond = pointer(float64(rx) / seconds)
		s.TransmitBytesPerSecond = pointer(float64(tx) / seconds)
	}
	return s
}

func rate(current, previous, seconds, scale float64) *float64 {
	if seconds <= 0 || current < previous || math.IsNaN(current) || math.IsNaN(previous) || math.IsInf(current, 0) || math.IsInf(previous, 0) {
		return nil
	}
	return pointer((current - previous) / seconds * scale)
}

func pointer[T any](value T) *T { return &value }

func histogramQuantile(h *runtimemetrics.Float64Histogram, q float64) *float64 {
	var total uint64
	for _, n := range h.Counts {
		total += n
	}
	if total == 0 {
		return nil
	}
	var cumulative uint64
	for i, n := range h.Counts {
		cumulative += n
		if float64(cumulative) >= float64(total)*q {
			lower := h.Buckets[i]
			if math.IsInf(lower, 0) || math.IsNaN(lower) || lower < 0 {
				return nil
			}
			return pointer(lower)
		}
	}
	return nil
}
