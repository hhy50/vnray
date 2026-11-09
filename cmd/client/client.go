//go:build !vnray_small

package main

import (
	"github.com/spf13/cobra"
)

// NewClientCommand 构造客户端根命令。
func NewClientCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "client",
		Short:         "连接服务端，接收请求并在本地路由到目标",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE:          runClient,
	}
	registerConfigFlag(cmd)
	cmd.Flags().String("server", "", "覆盖配置文件：请求发起服务端地址")
	return cmd
}

// RunClient 执行客户端命令，返回的错误已由 cobra 打印。
func RunClient() error {
	return NewClientCommand().Execute()
}

func runClient(cmd *cobra.Command, _ []string) error {
	return runConfiguredClient(clientOptions{
		configPath: configPath(cmd),
		serverAddr: mustGetString(cmd, "server"),
		serverSet:  cmd.Flags().Changed("server"),
	})
}
