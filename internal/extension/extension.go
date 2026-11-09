package extension

import "context"

// Service 是系统的一个组成部分，生命周期与进程一致，因此没有 Stop。
// 持有后台任务的实现另需实现 Drainer，收尾时由 Instance 等待。
type Service interface {
	Name() string
	Start(ctx context.Context) error
}

// Drainer 由"退出前需要等在途工作收尾"的服务可选实现。
type Drainer interface {
	Drain(ctx context.Context) error
}
