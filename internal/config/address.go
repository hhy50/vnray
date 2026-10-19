package config

import (
	"fmt"
	"net"
	"strconv"
)

func splitHostPort(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("invalid host:port %q: %w", address, err)
	}
	return host, port, nil
}

func validPortText(portText string) bool {
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port < 65536
}
