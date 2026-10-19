package extension

import "context"

// Service 是系统的一个组成部分，生命周期与进程一致，因此没有 Stop。
type Service interface {
	Name() string
	Start(ctx context.Context) error
}

// Drainer 由"退出前需要等在途工作收尾"的服务可选实现。
type Drainer interface {
	Drain(ctx context.Context) error
}

// Startable 是可任意启停单元的公共契约：Start 非阻塞，Stop 幂等。
type Startable interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Component 是服务内可独立启停的单元，归属于某个服务。
type Component interface{ Startable }

// Feature 是组件内可独立启停的最小能力单元（如 tcp 监听），归属于某个组件。
type Feature interface{ Startable }
