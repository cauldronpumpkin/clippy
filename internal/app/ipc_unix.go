//go:build !windows

package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"
)

func DefaultControlEndpoint() string {
	return filepath.Join(os.TempDir(), "clippy.sock")
}

func listenControl(endpoint string) (net.Listener, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o755); err != nil {
		return nil, nil, err
	}
	_ = os.Remove(endpoint)

	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		return nil, nil, err
	}

	return listener, func() error { return os.Remove(endpoint) }, nil
}

func dialControl(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", endpoint)
}

func pollInterval() time.Duration {
	return 200 * time.Millisecond
}
