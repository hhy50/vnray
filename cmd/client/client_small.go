//go:build vnray_small

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func parseSmallClient(args []string, output io.Writer) (clientOptions, error) {
	var opts clientOptions
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.configPath, "c", "", "配置文件路径（YAML）")
	flags.StringVar(&opts.configPath, "config", "", "配置文件路径（YAML）")
	flags.StringVar(&opts.serverAddr, "server", "", "覆盖配置文件：服务端地址")
	flags.BoolVar(&opts.version, "version", false, "打印客户端版本")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "server" {
			opts.serverSet = true
		}
	})
	return opts, nil
}

// RunClient 执行小体积客户端命令并打印最终错误。
func RunClient() error {
	return runSmallClient(os.Args[1:], os.Stdout, os.Stderr)
}

func runSmallClient(args []string, stdout, stderr io.Writer) error {
	opts, err := parseSmallClient(args, io.Discard)
	if errors.Is(err, flag.ErrHelp) {
		_, err = parseSmallClient([]string{"-h"}, stdout)
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err == nil {
		if opts.version {
			_, err = fmt.Fprintf(stdout, "client version %s\n", version)
			return err
		}
		err = runConfiguredClient(opts)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return err
}
