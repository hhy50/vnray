package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/hhy50/vnray/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

// logger 是全局句柄：Init 只在进程启动早期写一次，此后全是无锁读。
var logger atomic.Pointer[slog.Logger]

// levelVar 是全局日志级别：运行时可改，既有 handler 立即按新门槛过滤。
var levelVar slog.LevelVar

// dynamicWriter 是可替换输出目标的包装：日志轮转参数变更时换一个新的 lumberjack.Logger，
// 全局 handler 与 Scoped 日志器都经它访问当前 sink，故换文件对既有日志器自动可见，
// 无需重建。Write 持读锁，替换持写锁后再释放旧 sink——lumberjack 的字段不可并发写，
// 这是唯一安全的换文件方式。
type dynamicWriter struct {
	mu      sync.RWMutex
	w       io.Writer
	closer  io.Closer
	file    string
	console bool
	// stream 是标准流输出时的去向（"stdout"/"stderr"），file 非空时不参与。
	stream string
}

func (d *dynamicWriter) Write(p []byte) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.w.Write(p)
}

// swap 替换 sink 并释放旧的（标准输出没有 closer，保持原状）。
func (d *dynamicWriter) swap(w io.Writer, closer io.Closer, file string, console bool, stream string) {
	d.mu.Lock()
	old := d.closer
	d.w, d.closer, d.file, d.console, d.stream = w, closer, file, console, stream
	d.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// out 是 Init 选定的输出目标：Scoped 日志器据此构造"同格式、不同级别门槛"的日志器。
var out atomic.Pointer[dynamicWriter]

func init() {
	d := &dynamicWriter{w: os.Stderr}
	out.Store(d)
	logger.Store(slog.New(slog.NewTextHandler(d, &slog.HandlerOptions{Level: &levelVar})))
}

func newTextHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
}

// newFileSink 构造文件输出 sink（lumberjack 追加写与轮转）；console 为 true 时镜像到终端。
func newFileSink(path string, rotate config.LogRotation, console bool) (io.Writer, io.Closer) {
	file := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    rotate.MaxSizeMB,
		MaxBackups: rotate.MaxBackups,
		MaxAge:     rotate.MaxAgeDays,
	}
	if console {
		return io.MultiWriter(file, os.Stderr), file
	}
	return file, file
}

// Init 初始化全局日志：level 取 debug|info|warn|error；output 取 stderr|stdout 或
// 文件路径（lumberjack 追加写与轮转）；console 为 true 时文件输出镜像到终端。
func Init(cfg *config.Log) error {
	if cfg == nil {
		cfg = config.DefaultLog()
	}
	if err := SetLevel(cfg.Level); err != nil {
		return err
	}
	d := out.Load()
	if cfg.IsStdOutput() {
		if cfg.Output == "stdout" {
			d.swap(os.Stdout, nil, "", false, "stdout")
		} else {
			d.swap(os.Stderr, nil, "", false, "stderr")
		}
		return nil
	}
	rotate := config.LogRotation{
		MaxSizeMB:  config.DefaultLogRotateMaxSizeMB,
		MaxBackups: config.DefaultLogRotateMaxBackups,
		MaxAgeDays: config.DefaultLogRotateMaxAgeDays,
	}
	w, closer := newFileSink(cfg.Output, rotate, cfg.Console)
	d.swap(w, closer, cfg.Output, cfg.Console, "")
	return nil
}

// OutputTarget 返回日志的实际去向，供启动横幅与排障使用：标准流返回 "stderr"/"stdout"，
// 文件输出返回绝对路径（相对路径按进程工作目录展开）。
func OutputTarget() string {
	d := out.Load()
	d.mu.RLock()
	file, stream := d.file, d.stream
	d.mu.RUnlock()
	if file == "" {
		if stream == "" {
			return "stderr"
		}
		return stream
	}
	if abs, err := filepath.Abs(file); err == nil {
		return abs
	}
	return file
}

// MirrorsConsole 报告日志是否同时出现在终端：标准流输出，或文件输出且开了 console 镜像。
func MirrorsConsole() bool {
	d := out.Load()
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.file == "" || d.console
}

// SetLevel 热更新全局日志级别；会话级（Scoped）日志器有各自门槛，不受影响。
func SetLevel(raw string) error {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(raw)); err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	levelVar.Set(lv)
	return nil
}

// SetLogRotation 热更新日志文件轮转参数：仅当日志输出到文件时生效，否则返回错误。
func SetLogRotation(rotate config.LogRotation) error {
	d := out.Load()
	d.mu.RLock()
	path, console := d.file, d.console
	d.mu.RUnlock()
	if path == "" {
		return errors.New("logging: log output is not a file, rotation settings have no effect")
	}
	w, closer := newFileSink(path, rotate, console)
	d.swap(w, closer, path, console, "")
	return nil
}

// Sync 刷新底层缓冲；文本 handler 无缓冲，保留以满足退出路径的调用习惯。
func Sync() error { return nil }

func log(level slog.Level, msg string, args ...any) {
	logger.Load().Log(context.Background(), level, msg, args...)
}

// Debugf 以 debug 级别记录一条格式化消息。
func Debugf(format string, args ...any) { log(slog.LevelDebug, fmt.Sprintf(format, args...)) }

// Infof 以 info 级别记录一条格式化消息。
func Infof(format string, args ...any) { log(slog.LevelInfo, fmt.Sprintf(format, args...)) }

// Warnf 以 warn 级别记录一条格式化消息。
func Warnf(format string, args ...any) { log(slog.LevelWarn, fmt.Sprintf(format, args...)) }

// Errorf 以 error 级别记录一条格式化消息。
func Errorf(format string, args ...any) { log(slog.LevelError, fmt.Sprintf(format, args...)) }

// Debug 记录一条带键值对的 debug 消息（参数按 key, value 交替给出）。
func Debug(msg string, args ...any) { log(slog.LevelDebug, msg, args...) }

// Info 记录一条带键值对的 info 消息（参数按 key, value 交替给出）。
func Info(msg string, args ...any) { log(slog.LevelInfo, msg, args...) }

// Warn 记录一条带键值对的 warn 消息（参数按 key, value 交替给出）。
func Warn(msg string, args ...any) { log(slog.LevelWarn, msg, args...) }

// Error 记录一条带键值对的 error 消息（参数按 key, value 交替给出）。
func Error(msg string, args ...any) { log(slog.LevelError, msg, args...) }

// Scoped 是会话级日志器：与全局日志共用输出目标与文本格式，但自带级别门槛，
// 不受 logging.Init 的全局级别约束（全局级别只管全局日志）。零值或 nil 可安全调用。
type Scoped struct{ l *slog.Logger }

// NewScoped 按 level（debug|info|warn|error；空或非法取 info）构造日志器，
// attrs 附在每条日志上，通常放 user / client 这类身份字段。
func NewScoped(level string, attrs ...any) *Scoped {
	lvl := slog.LevelInfo
	if level != "" {
		var parsed slog.Level
		if err := parsed.UnmarshalText([]byte(level)); err == nil {
			lvl = parsed
		}
	}
	var sink io.Writer = os.Stderr
	if d := out.Load(); d != nil {
		sink = d
	}
	l := slog.New(newTextHandler(sink, lvl))
	if len(attrs) > 0 {
		l = l.With(attrs...)
	}
	return &Scoped{l: l}
}

func (s *Scoped) log(level slog.Level, format string, args ...any) {
	if s == nil || s.l == nil {
		return
	}
	s.l.Log(context.Background(), level, fmt.Sprintf(format, args...))
}

// Debugf 以 debug 级别记录一条格式化消息。
func (s *Scoped) Debugf(format string, args ...any) { s.log(slog.LevelDebug, format, args...) }

// Infof 以 info 级别记录一条格式化消息。
func (s *Scoped) Infof(format string, args ...any) { s.log(slog.LevelInfo, format, args...) }

// Warnf 以 warn 级别记录一条格式化消息。
func (s *Scoped) Warnf(format string, args ...any) { s.log(slog.LevelWarn, format, args...) }

// Errorf 以 error 级别记录一条格式化消息。
func (s *Scoped) Errorf(format string, args ...any) { s.log(slog.LevelError, format, args...) }

func (s *Scoped) logArgs(level slog.Level, msg string, args ...any) {
	if s == nil || s.l == nil {
		return
	}
	s.l.Log(context.Background(), level, msg, args...)
}

// Debug 记录一条带键值对的 debug 消息（参数按 key, value 交替给出）。
func (s *Scoped) Debug(msg string, args ...any) { s.logArgs(slog.LevelDebug, msg, args...) }

// Info 记录一条带键值对的 info 消息。
func (s *Scoped) Info(msg string, args ...any) { s.logArgs(slog.LevelInfo, msg, args...) }

// Warn 记录一条带键值对的 warn 消息。
func (s *Scoped) Warn(msg string, args ...any) { s.logArgs(slog.LevelWarn, msg, args...) }

// Error 记录一条带键值对的 error 消息。
func (s *Scoped) Error(msg string, args ...any) { s.logArgs(slog.LevelError, msg, args...) }
