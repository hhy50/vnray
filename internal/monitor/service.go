package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/pipe"
	"github.com/hhy50/vnray/internal/routine"
	"github.com/hhy50/vnray/internal/transport"
)

const storageTimeout = 5 * time.Second

// ErrDisabled 表示监控采样未启用。
var ErrDisabled = errors.New("monitoring is disabled")

// ErrWarming 表示尚无当前采集周期内的新鲜快照。
var ErrWarming = errors.New("monitoring sample is warming up")

type publication struct {
	host                Host
	users               Response
	hostJSON, usersJSON []byte
	queryGate           chan struct{}
	queries             map[Query]cachedQuery
}

type cachedQuery struct {
	data    []byte
	expires time.Time
}

// Service 拥有唯一采样协程；只读快照通过原子指针发布。
type Service struct {
	opts           Options
	settings       atomic.Pointer[Settings]
	current        atomic.Pointer[publication]
	reload         chan struct{}
	userVersion    atomic.Uint64
	users          []UserSource
	loadedVersion  uint64
	started        time.Time
	lastMaintained time.Time
	lastRetention  time.Duration
	historyOnce    sync.Once
	historyErr     error
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	stopped        bool
}

// New 创建尚未启动的监控模块。
func New(opts Options) *Service {
	s := &Service{opts: opts, reload: make(chan struct{}, 1), started: time.Now()}
	s.settings.Store(&Settings{Interval: 2 * time.Second, Retention: time.Hour})
	s.userVersion.Store(1)
	return s
}

// Configure 发布配置并唤醒采样器；事件指标与监控共享总开关。
func (s *Service) Configure(settings Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *s.settings.Load() == settings {
		return
	}
	metrics.SetEnabled(settings.Enabled)
	s.settings.Store(&settings)
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// InvalidateUsers 通知目录变化，下一次采样重新加载，不在写请求中扫库。
func (s *Service) InvalidateUsers() { s.userVersion.Add(1) }

// Name 返回生命周期服务名。
func (s *Service) Name() string { return "MonitoringService" }

// Start 启动采样，后台任务由 Drain 取消并等待。
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil || s.stopped {
		return errors.New("monitoring: service already started or stopped")
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	routine.Go("monitor.sample", func() {
		defer close(s.done)
		defer s.closeHistory()
		s.run(ctx)
	})
	return nil
}

// Drain 幂等地终止采样并等待历史存储关闭。
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		s.closeHistory()
		return s.historyErr
	}
	cancel()
	select {
	case <-done:
		return s.historyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) closeHistory() {
	s.historyOnce.Do(func() {
		if s.opts.History != nil {
			s.historyErr = s.opts.History.Close()
		}
	})
}

func (s *Service) run(ctx context.Context) {
	sampler := metrics.NewSampler()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var nextSample time.Time
	var applied Settings
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reload:
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.reload:
		default:
		}
		settings := *s.settings.Load()
		if settings != applied {
			nextSample = time.Time{}
			applied = settings
		}
		if current := s.current.Load(); current != nil && !current.host.Current() {
			nextSample = time.Time{}
		}
		if settings.Enabled && !time.Now().Before(nextSample) {
			nextSample = time.Now().Add(settings.Interval)
			s.collect(ctx, sampler, settings)
		} else if !settings.Enabled {
			sampler.Reset()
			s.current.Store(nil)
		}
		if s.opts.History != nil && (s.lastRetention != settings.Retention || time.Since(s.lastMaintained) >= time.Minute) {
			s.maintainHistory(ctx, settings.Retention)
		}
		wait := time.Minute
		if settings.Enabled {
			wait = min(wait, max(time.Millisecond, time.Until(nextSample)))
		}
		if s.opts.History != nil && !s.lastMaintained.IsZero() {
			wait = min(wait, max(time.Millisecond, time.Until(s.lastMaintained.Add(time.Minute))))
		}
		timer.Reset(wait)
	}
}

func (s *Service) loadUsers(ctx context.Context) error {
	version := s.userVersion.Load()
	if s.opts.Users == nil || s.loadedVersion == version {
		return nil
	}
	users, err := s.opts.Users(ctx)
	if err != nil {
		return err
	}
	s.users, s.loadedVersion = users, version
	return nil
}

func (s *Service) collect(ctx context.Context, sampler *metrics.Sampler, settings Settings) {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	if err := s.loadUsers(ctx); err != nil {
		logging.Warn("monitoring: load users", "err", err)
		return
	}
	host := Host{Sample: sampler.Collect(), UptimeSeconds: int64(time.Since(s.started).Seconds()), IntervalSeconds: settings.Interval.Seconds(), Transfer: transport.TransferSnapshot()}
	host.PipeAllocations, host.PipeAllocatedBytes = pipe.BufferAllocations()
	p := &publication{host: host, queryGate: make(chan struct{}, 1), queries: make(map[Query]cachedQuery), users: Response{
		Snapshot:        Snapshot{SampledAt: host.SampledAt, Users: s.sampleUsers(host.SampledAt), Server: host.Server, Process: host.Process},
		IntervalSeconds: settings.Interval.Seconds(), RetentionSeconds: settings.Retention.Seconds(), History: []Snapshot{},
	}}
	var err error
	p.hostJSON, err = json.Marshal(host)
	if err != nil {
		logging.Warn("monitoring: encode host", "err", err)
		return
	}
	if s.opts.History != nil {
		err = s.opts.History.Save(ctx, p.users.Snapshot)
		p.users.HistoryStored = err == nil
		if err != nil {
			logging.Warn("monitoring: persist sample", "err", err)
		}
	}
	p.usersJSON, err = json.Marshal(p.users)
	if err != nil {
		logging.Warn("monitoring: encode users", "err", err)
		return
	}
	s.current.Store(p)
}

func (s *Service) sampleUsers(at time.Time) []User {
	previous := s.current.Load()
	baseline := make(map[uint64]User)
	var seconds float64
	if previous != nil && previous.host.Current() {
		seconds = at.Sub(previous.users.SampledAt).Seconds()
		for _, user := range previous.users.Users {
			baseline[user.ID] = user
		}
	}
	users := make([]User, 0, len(s.users))
	for _, source := range s.users {
		u := User{ID: source.ID, Name: source.Name, Protocol: source.Protocol}
		if stats := source.Stats; stats != nil {
			u.TCP, u.Streams, u.Requests = stats.TCP.Load(), stats.Streams.Load(), stats.Requests.Load()
			u.Up, u.Down = stats.UpWrite.Load(), stats.DownWrite.Load()
		}
		if old, ok := baseline[u.ID]; ok && seconds > 0 && u.Up >= old.Up && u.Down >= old.Down {
			up, down := float64(u.Up-old.Up)/seconds, float64(u.Down-old.Down)/seconds
			u.UpRate, u.DownRate = &up, &down
		}
		users = append(users, u)
	}
	return users
}

func (s *Service) maintainHistory(ctx context.Context, retention time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	s.lastMaintained = time.Now()
	if err := s.opts.History.Configure(ctx, retention); err != nil {
		logging.Warn("monitoring: configure history", "err", err)
		return
	}
	s.lastRetention = retention
}
