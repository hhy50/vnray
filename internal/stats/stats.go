// Package stats 承载跨组件共享的计数器类型：transport（泵）、ratelimit（注册表）与
// traffic（落库）都要引用它，放在任一使用方都会把依赖方向带偏，故单独成包。
package stats

import "sync/atomic"

// PumpStats 汇集原子的连接、请求与双向载荷计数，可跨协程读写。
// Up* 对应 a→b 方向（调用方约定 a 是用户侧），Down* 对应 b→a。
type PumpStats struct {
	TCP       atomic.Int64
	Streams   atomic.Int64
	Requests  atomic.Int64
	UpRead    atomic.Int64
	UpWrite   atomic.Int64
	DownRead  atomic.Int64
	DownWrite atomic.Int64
}

// AddTCP 配对更新已认证的物理连接数。
func (s *PumpStats) AddTCP(delta int64) {
	if s != nil {
		s.TCP.Add(delta)
	}
}

// OpenStream 登记逻辑流，返回原指针供 defer CloseStream 配对，无闭包分配。
func (s *PumpStats) OpenStream() *PumpStats {
	if s != nil {
		s.Streams.Add(1)
	}
	return s
}

// CloseStream 结束一次 OpenStream 登记。
func (s *PumpStats) CloseStream() {
	if s != nil {
		s.Streams.Add(-1)
	}
}

// OpenTCP 登记物理连接，返回原指针供 defer CloseTCP 配对。
func (s *PumpStats) OpenTCP() *PumpStats { s.AddTCP(1); return s }

// CloseTCP 结束一次 OpenTCP 登记。
func (s *PumpStats) CloseTCP() { s.AddTCP(-1) }

// Request 记录一次代理目标请求，包含拒绝和失败。
func (s *PumpStats) Request() {
	if s != nil {
		s.Requests.Add(1)
	}
}

// 四个取址方法都 nil 安全：Stats 为 nil（未启用统计）时管线拿到空指针、直接跳过计数。

// UpReadRef 返回上行读计数的地址。
func (s *PumpStats) UpReadRef() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.UpRead
}

// UpWriteRef 返回上行写计数的地址。
func (s *PumpStats) UpWriteRef() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.UpWrite
}

// DownReadRef 返回下行读计数的地址。
func (s *PumpStats) DownReadRef() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.DownRead
}

// DownWriteRef 返回下行写计数的地址。
func (s *PumpStats) DownWriteRef() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.DownWrite
}
