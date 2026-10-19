package cli

// 客户端与服务端现在同处一个模块，隔离只剩依赖方向这一道约束：client 侧一旦链进
// 服务端专用依赖，SQLite 驱动的 init 就会把整个纯 Go 引擎打进 client 二进制
// （当初拆成两个项目就是为了这件事）。本测试是那条约束的闸门。

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenClientDeps 是 client 依赖闭包里绝不允许出现的包前缀。
var forbiddenClientDeps = []string{
	"github.com/hhy50/vnray/admin",
	"github.com/hhy50/vnray/internal/core",
	"github.com/hhy50/vnray/internal/store",
	"gorm.io/",
	"github.com/glebarez/",
	"modernc.org/sqlite",
	"modernc.org/libc",
}

// TestClientBinaryFreeOfServerDeps 断言 cmd/client 的依赖闭包不含服务端专用包。
func TestClientBinaryFreeOfServerDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/client")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/client: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, forbidden := range forbiddenClientDeps {
			if dep == forbidden || strings.HasPrefix(dep, forbidden) {
				t.Errorf("cmd/client 依赖了服务端专用包 %s；这会把它链进 client 二进制", dep)
			}
		}
	}
}
