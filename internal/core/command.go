package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
)

// version 由构建参数（-ldflags "-X .../internal/serverapp.version=x.y.z"）覆盖，
// 默认 dev 用于本地 go run；与 internal/cli.version 各自独立。
var version = "dev"

// registerConfigFlag 注册配置文件路径 flag；日志不提供命令行覆盖，统一在配置文件的 log 段设置。
func registerConfigFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("config", "c", "", "配置文件路径（YAML）；为空时使用内置默认值")
}

func configPath(cmd *cobra.Command) string {
	v, err := cmd.Flags().GetString("config")
	if err != nil {
		panic(err)
	}
	return v
}

// NewServerCommand 构造服务端根命令。
func NewServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "server",
		Short:         "监听代理客户端连接并保持会话",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE:          runServer,
	}
	registerConfigFlag(cmd)
	return cmd
}

// RunServer 执行服务端命令，返回的错误已由 cobra 打印。
func RunServer() error {
	return NewServerCommand().Execute()
}

func runServer(cmd *cobra.Command, _ []string) error {
	cfg := config.DefaultServer()
	if path := configPath(cmd); path != "" {
		if err := config.Load(path, &cfg); err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	if err := logging.Init(cfg.Log); err != nil {
		return err
	}

	appctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := initServerApp(appctx, &cfg)
	if err != nil {
		return fmt.Errorf("assemble server: %w", err)
	}
	if err := app.Run(); err != nil {
		logging.Errorf("server stopped: %v", err)
		return err
	}
	return nil
}
