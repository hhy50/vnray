package admin

import (
	"sync"
	"time"

	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/transport"
)

// sample 是仪表盘曲线的一个采样点：Traffic 是累计转发字节的读数，曲线做差分得速率。
type sample struct {
	At      time.Time
	Clients int
	Tunnels int
	Traffic int64
}

const (
	sampleInterval = 10 * time.Second
	// sampleWindow 是环形缓冲的点数上限：120 点 × 10s 正好是图表展示的 20 分钟。
	sampleWindow = 120
)

// sampler 是进程内的指标环形缓冲。采样不落库：图表只需要最近 20 分钟，
// 存库既多一张表又每 10s 一次写，进程重启丢掉这点历史无影响。
type sampler struct {
	mu   sync.Mutex
	buf  []sample
	next int
	n    int
}

func newSampler() *sampler { return &sampler{buf: make([]sample, sampleWindow)} }

func (s *sampler) add(x sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.next] = x
	s.next = (s.next + 1) % sampleWindow
	if s.n < sampleWindow {
		s.n++
	}
}

// recent 返回按时间升序的全部有效采样点。
func (s *sampler) recent() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sample, 0, s.n)
	start := (s.next - s.n + sampleWindow) % sampleWindow
	for i := range s.n {
		out = append(out, s.buf[(start+i)%sampleWindow])
	}
	return out
}

// startSampler 启动周期采样，返回幂等的停止函数；先立刻采一次，避免首图空 10 秒。
func startSampler(s *sampler, clients *transport.SessionManager) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	routine.Go("admin.metrics.sampler", func() {
		sampleOnce(s, clients)
		t := time.NewTicker(sampleInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sampleOnce(s, clients)
			}
		}
	})
	return func() { once.Do(func() { close(done) }) }
}

func sampleOnce(s *sampler, clients *transport.SessionManager) {
	traffic, active := transport.TunnelStats()
	s.add(sample{At: time.Now(), Clients: clients.ClientCount(), Tunnels: int(active), Traffic: traffic})
}
