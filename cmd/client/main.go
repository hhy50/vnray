package main

import (
	"os"

	"github.com/hhy50/vnray/internal/cli"
)

func main() {
	// 命令构造、配置装配、依赖装配与错误打印都在 internal/cli。
	if err := cli.RunClient(); err != nil {
		os.Exit(1)
	}
}
