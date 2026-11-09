package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
)

type clientOptions struct {
	configPath string
	serverAddr string
	serverSet  bool
	version    bool
}

func runConfiguredClient(opts clientOptions) error {
	cfg := config.DefaultClient()
	if opts.configPath != "" {
		if err := config.Load(opts.configPath, &cfg); err != nil {
			return err
		}
	}
	if opts.serverSet {
		cfg.Server.Addr = opts.serverAddr
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
	// Signals must interrupt both reconnect backoff and an active session.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		logging.Errorf("client stopped: %v", err)
		return err
	}
	logging.Infof("server session closed; all requests finished")
	return nil
}
