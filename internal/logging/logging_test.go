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
