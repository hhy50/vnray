package main

import (
	"os"

	app "github.com/hhy50/vnray/internal/core"

	// 前端构建产物经 init() 注册进管理台；用占位 dist 时构建不依赖 Node。
	_ "github.com/hhy50/vnray/web/admin"
)

func main() {
	if err := app.RunServer(); err != nil {
		os.Exit(1)
	}
}
