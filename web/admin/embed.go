// Package webadmin 把 Vue 管理台的构建产物带进二进制。
//
// 它只做一件事：把 dist 目录注册给 admin 包。cmd/server 空导入本包即可生效，
// admin 包本身不依赖前端产物（单元测试照常跑）。
package webadmin

import (
	"embed"
	"io/fs"

	"github.com/hhy50/vnray/admin"
)

// dist/index.html 单独列一条是有意的：没有构建产物时 go build 直接报
// "pattern dist/index.html: no matching files found"，而不是编出一个没有界面的二进制。
//
//go:embed all:dist dist/index.html
var assets embed.FS

func init() {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		return
	}
	admin.RegisterUI(sub)
}
