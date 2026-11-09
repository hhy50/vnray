// Package stats 承载跨组件共享的计数器类型：transport（泵）、ratelimit（注册表）与
// traffic（落库）都要引用它，放在任一使用方都会把依赖方向带偏，故单独成包。
package stats

import "sync/atomic"

// PumpStats 汇集双向搬运的字节统计：四个计数都是原子的，可跨协程读写。
// Up* 对应 a→b 方向（调用方约定 a 是用户侧），Down* 对应 b→a。
type PumpStats struct {
	UpRead    atomic.Int64
	UpWrite   atomic.Int64
	DownRead  atomic.Int64
	DownWrite atomic.Int64
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
