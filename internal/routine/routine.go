package routine

import (
	"runtime/debug"

	"github.com/hhy50/vnray/internal/logging"
)

// Go 启动一个带 recover 的协程；name 用于 panic 日志定位
// （建议用"包.组件.职责"命名，如 "server.client.upstream"）。
func Go(name string, fn func()) {
	go run(name, fn)
}

// run 执行 fn 并捕获 panic；recover 后进程继续运行。
func run(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			panicHandler(name, r, debug.Stack())
		}
	}()
	fn()
}

// panicHandler 是 panic 的统一出口；包级变量仅供本包测试注入，
// 生产代码使用默认实现（Error 日志 + 堆栈），不应替换。
var panicHandler = func(name string, r any, stack []byte) {
	logging.Error("routine: goroutine panicked",
		"goroutine", name,
		"panic", r,
		"stack", stack)
}
