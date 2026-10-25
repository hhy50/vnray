package logging

import (
	"path/filepath"
	"testing"

	"github.com/hhy50/vnray/internal/config"
)

// TestSetLevel 断言全局日志级别运行时可改，非法级别被拒。
func TestSetLevel(t *testing.T) {
	for _, lv := range []string{"debug", "info", "warn", "error"} {
		if err := SetLevel(lv); err != nil {
			t.Fatalf("SetLevel(%q): %v", lv, err)
		}
	}
	if err := SetLevel("verbose"); err == nil {
		t.Fatal("SetLevel accepted an invalid level")
	}
}

// TestOutputTarget 断言日志去向可被启动横幅读出：文件输出给绝对路径，标准流给名字。
func TestOutputTarget(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.log")
	if err := Init(&config.Log{Level: "info", Output: file}); err != nil {
		t.Fatalf("init file output: %v", err)
	}
	if got := OutputTarget(); got != file {
		t.Fatalf("OutputTarget() = %q, want %q", got, file)
	}
	if MirrorsConsole() {
		t.Fatal("file output without console should not mirror the console")
	}

	if err := Init(&config.Log{Level: "info", Output: file, Console: true}); err != nil {
		t.Fatalf("init mirrored file output: %v", err)
	}
	if !MirrorsConsole() {
		t.Fatal("console mirror should be reported")
	}

	if err := Init(&config.Log{Level: "info", Output: "stdout"}); err != nil {
		t.Fatalf("init stdout output: %v", err)
	}
	if got := OutputTarget(); got != "stdout" {
		t.Fatalf("OutputTarget() = %q, want stdout", got)
	}
	if !MirrorsConsole() {
		t.Fatal("stdout output should mirror the console")
	}
}

// TestSetLogRotation 断言轮转参数仅在日志输出到文件时可改，其余情况返回错误。
func TestSetLogRotation(t *testing.T) {
	rotate := config.LogRotation{MaxSizeMB: 10, MaxBackups: 2, MaxAgeDays: 1}

	if err := Init(&config.Log{Level: "info", Output: filepath.Join(t.TempDir(), "app.log")}); err != nil {
		t.Fatalf("init file output: %v", err)
	}
	if err := SetLogRotation(rotate); err != nil {
		t.Fatalf("SetLogRotation on file output: %v", err)
	}

	if err := Init(&config.Log{Level: "info", Output: "stderr"}); err != nil {
		t.Fatalf("init stderr output: %v", err)
	}
	if err := SetLogRotation(rotate); err == nil {
		t.Fatal("SetLogRotation should fail when output is not a file")
	}
}
