package routine

import (
	"runtime/debug"
	"sync"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/metrics"
)

// gauges 缓存 name 到活跃协程读数的映射：name 在各调用点都是字面量，
// 集合有限，缓存避免每次 Go 都拼指标名。
var gauges sync.Map

// panicTotal 统计被 recover 的 panic 次数。
var panicTotal = metrics.NewCounter("routine_panics_total")

// Go 启动一个带 recover 的协程；name 用于 panic 日志定位
// （建议用"包.组件.职责"命名，如 "server.client.upstream"）。
func Go(name string, fn func()) {
	g := gaugeFor(name)
	g.Add(1)
	go run(name, fn, g)
}

// run 执行 fn 并捕获 panic；recover 后进程继续运行。
func run(name string, fn func(), active *metrics.Gauge) {
	defer active.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			panicTotal.Inc()
			panicHandler(name, r, debug.Stack())
		}
	}()
	fn()
}

// gaugeFor 返回该 name 的活跃协程读数，按需注册为
// routine_goroutines{goroutine="name"}。
func gaugeFor(name string) *metrics.Gauge {
	if v, ok := gauges.Load(name); ok {
		return v.(*metrics.Gauge)
	}
	g := metrics.GaugeFor(`routine_goroutines{goroutine="` + name + `"}`)
	v, _ := gauges.LoadOrStore(name, g)
	return v.(*metrics.Gauge)
}

// panicHandler 是 panic 的统一出口；包级变量仅供本包测试注入，
// 生产代码使用默认实现（Error 日志 + 堆栈），不应替换。
var panicHandler = func(name string, r any, stack []byte) {
	logging.Error("routine: goroutine panicked",
		"goroutine", name,
		"panic", r,
		"stack", stack)
}
