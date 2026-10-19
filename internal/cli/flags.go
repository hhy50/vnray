package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// registerConfigFlag 注册配置文件路径 flag。
// 日志不提供命令行覆盖，统一在配置文件的 log 段设置。
func registerConfigFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("config", "c", "", "配置文件路径（YAML）；为空时使用内置默认值")
}

func configPath(cmd *cobra.Command) string {
	return mustGetString(cmd, "config")
}

// mustGetString 读取确定已注册的 string flag；未注册只可能是编程错误。
func mustGetString(cmd *cobra.Command, name string) string {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		panic(fmt.Sprintf("flag %q not registered: %v", name, err))
	}
	return v
}
