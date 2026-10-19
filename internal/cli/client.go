package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
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
	cfg := config.DefaultClient()
	if path := configPath(cmd); path != "" {
		if err := config.Load(path, &cfg); err != nil {
			return err
		}
	}
	if cmd.Flags().Changed("server") {
		cfg.Server.Addr = mustGetString(cmd, "server")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if err := logging.Init(cfg.Log); err != nil {
		return err
	}

	app, err := newClientApp(cfg)
	if err != nil {
		return fmt.Errorf("assemble client: %w", err)
	}
	// 客户端默认长驻重连，退出只能靠信号：Ctrl+C 需要打断退避等待并关闭会话，
	// 而不是让进程卡在 sleep 里。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		logging.Errorf("client stopped: %v", err)
		return err
	}
	logging.Infof("server session closed; all requests finished")
	return nil
}
