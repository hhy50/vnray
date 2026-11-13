// Package monitor 管理主机与用户监控的采样、历史和只读快照，不进入转发路径。
package monitor

import (
	"context"
	"time"

	"github.com/hhy50/vnray/internal/metrics"
	"github.com/hhy50/vnray/internal/stats"
	"github.com/hhy50/vnray/internal/transport"
)

// Settings 是可热更新的采样策略，取值由配置层校验。
type Settings struct {
	Enabled   bool
	Interval  time.Duration
	Retention time.Duration
}

// UserSource 是用户目录的不可变投影，Stats 在连接之间共享。
type UserSource struct {
	ID       uint64
	Name     string
	Protocol string
	Stats    *stats.PumpStats
}

// HistoryStore 保存数值时序并重建快照，由 Service 配置和关闭。
type HistoryStore interface {
	Save(context.Context, Snapshot) error
	Read(context.Context, time.Time, time.Time, int, []uint64) ([]Snapshot, error)
	Configure(context.Context, time.Duration) error
	Close() error
}

// Options 注入用户目录与历史存储，缺省时仍提供主机监控。
type Options struct {
	Users   func(context.Context) ([]UserSource, error)
	History HistoryStore
}

// User 是一次用户采样，累计值从进程启动起计。
type User struct {
	ID       uint64   `json:"id"`
	Name     string   `json:"name"`
	Protocol string   `json:"protocol"`
	TCP      int64    `json:"tcp"`
	Streams  int64    `json:"streams"`
	Requests int64    `json:"requests"`
	Up       int64    `json:"up"`
	Down     int64    `json:"down"`
	UpRate   *float64 `json:"up_rate"`
	DownRate *float64 `json:"down_rate"`
}

// Host 是主机与数据面全局指标的采样。
type Host struct {
	metrics.Sample
	UptimeSeconds      int64                  `json:"uptime_seconds"`
	IntervalSeconds    float64                `json:"interval_seconds"`
	Transfer           transport.TransferInfo `json:"transfer"`
	PipeAllocations    uint64                 `json:"pipe_allocations"`
	PipeAllocatedBytes uint64                 `json:"pipe_allocated_bytes"`
}

// Snapshot 是持久化和历史查询共用的采样值。
type Snapshot struct {
	SampledAt time.Time           `json:"sampled_at"`
	Users     []User              `json:"users"`
	Server    metrics.ServerInfo  `json:"server"`
	Process   metrics.ProcessInfo `json:"process"`
}

// Response 是最新采样以及可选的历史。
type Response struct {
	Snapshot
	IntervalSeconds  float64    `json:"interval_seconds"`
	RetentionSeconds float64    `json:"retention_seconds"`
	HistoryStored    bool       `json:"history_stored"`
	History          []Snapshot `json:"history"`
}

// Query 选择历史窗口；Points 为零时只返回预编码的最新采样。
type Query struct {
	Window time.Duration
	Points int
}
