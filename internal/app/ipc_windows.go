//go:build windows

package app

import (
	"context"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

func DefaultControlEndpoint() string {
	user := os.Getenv("USERNAME")
	if user == "" {
		user = "default"
	}
	user = strings.ReplaceAll(user, " ", "_")
	return `\\.\pipe\clippy-` + user
}

func listenControl(endpoint string) (net.Listener, func() error, error) {
	listener, err := winio.ListenPipe(endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	return listener, func() error { return nil }, nil
}

func dialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

func pollInterval() time.Duration {
	return 200 * time.Millisecond
}
