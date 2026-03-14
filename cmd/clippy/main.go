package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"clippy/internal/app"
)

const startTimeout = 10 * time.Second

type daemonController struct {
	runtime *app.Runtime
	stop    context.CancelFunc
}

func (d *daemonController) Status() app.Status {
	return d.runtime.Status()
}

func (d *daemonController) Stop() {
	d.stop()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "start":
		return runStart(args[1:])
	case "stop":
		return runStop(args[1:])
	case "status":
		return runStatus(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	default:
		return usageError()
	}
}

func runStart(args []string) error {
	cfg, foreground, err := parseCommonFlags("start", args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	if status, err := app.RequestControl(ctx, cfg.ControlEndpoint, "status"); err == nil && status != nil && status.Running {
		fmt.Printf("clippy is already running: name=%s node_id=%s\n", status.Name, status.NodeID)
		return nil
	}

	if foreground {
		return serveDaemon(cfg, true)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	argv := append([]string{exe, "daemon"}, daemonArgs(cfg)...)
	if err := app.SpawnDetached(argv, os.Environ()); err != nil {
		return err
	}

	status, err := waitForDaemon(context.Background(), cfg.ControlEndpoint, startTimeout)
	if err != nil {
		return err
	}

	fmt.Printf("clippy started: name=%s node_id=%s\n", status.Name, status.NodeID)
	return nil
}

func runStop(args []string) error {
	cfg, _, err := parseCommonFlags("stop", args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := app.RequestControl(ctx, cfg.ControlEndpoint, "stop")
	if err != nil {
		if isControlUnavailable(err) {
			fmt.Println("clippy is not running")
			return nil
		}
		return err
	}

	fmt.Printf("clippy stopping: name=%s node_id=%s\n", status.Name, status.NodeID)
	return nil
}

func runStatus(args []string) error {
	cfg, _, err := parseCommonFlags("status", args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := app.RequestControl(ctx, cfg.ControlEndpoint, "status")
	if err != nil {
		if isControlUnavailable(err) {
			fmt.Println("running: no")
			return nil
		}
		return err
	}

	fmt.Println("running: yes")
	fmt.Printf("name: %s\n", status.Name)
	fmt.Printf("node_id: %s\n", status.NodeID)
	fmt.Printf("started_at: %s\n", status.StartedAt.Format(time.RFC3339))
	fmt.Printf("peers: %d\n", len(status.Peers))
	for _, peer := range status.Peers {
		fmt.Printf("- %s (%s) host=%s port=%d connected=%t\n", peer.ID, peer.Name, peer.Host, peer.Port, peer.Connected)
	}
	return nil
}

func runDaemon(args []string) error {
	cfg, _, err := parseCommonFlags("daemon", args)
	if err != nil {
		return err
	}

	return serveDaemon(cfg, false)
}

func serveDaemon(cfg app.Config, foreground bool) error {
	logger := log.New(os.Stdout, "clippy: ", log.LstdFlags)
	_ = foreground

	rt, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rt.Start(ctx); err != nil {
		return err
	}

	server, err := app.NewControlServer(cfg.ControlEndpoint, &daemonController{runtime: rt, stop: stop}, logger)
	if err != nil {
		stop()
		_ = rt.Wait()
		return err
	}
	defer server.Close()

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.Serve(ctx)
	}()

	var serverErr error
	select {
	case serverErr = <-serverErrCh:
		if serverErr != nil {
			stop()
		}
	case <-ctx.Done():
	}

	if err := rt.Wait(); err != nil {
		return err
	}

	if serverErr != nil {
		return serverErr
	}
	return <-serverErrCh
}

func parseCommonFlags(name string, args []string) (app.Config, bool, error) {
	cfg := app.Config{
		Name:            envString("CLIPPY_NAME", ""),
		ListenAddr:      envString("CLIPPY_LISTEN_ADDR", ""),
		Port:            envInt("CLIPPY_PORT", 0),
		ServiceName:     envString("CLIPPY_SERVICE_NAME", ""),
		Domain:          envString("CLIPPY_DOMAIN", ""),
		WebSocketPath:   envString("CLIPPY_WEBSOCKET_PATH", ""),
		ControlEndpoint: envString("CLIPPY_CONTROL_ENDPOINT", app.DefaultControlEndpoint()),
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&cfg.Name, "name", cfg.Name, "local node name")
	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "listen host")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	fs.StringVar(&cfg.ServiceName, "service-name", cfg.ServiceName, "mDNS service name")
	fs.StringVar(&cfg.Domain, "domain", cfg.Domain, "mDNS domain")
	fs.StringVar(&cfg.WebSocketPath, "websocket-path", cfg.WebSocketPath, "websocket path")
	fs.StringVar(&cfg.ControlEndpoint, "control", cfg.ControlEndpoint, "control socket or pipe")

	var foreground bool
	if name == "start" || name == "daemon" {
		fs.BoolVar(&foreground, "foreground", false, "run in foreground")
	}

	if err := fs.Parse(args); err != nil {
		return app.Config{}, false, err
	}

	return cfg.WithDefaults(), foreground, nil
}

func waitForDaemon(ctx context.Context, endpoint string, timeout time.Duration) (*app.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, err := app.RequestControl(ctx, endpoint, "status")
		if err == nil && status != nil && status.Running {
			return status, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("timed out waiting for daemon readiness")
			}
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func daemonArgs(cfg app.Config) []string {
	args := make([]string, 0, 12)
	if cfg.Name != "" {
		args = append(args, "--name", cfg.Name)
	}
	if cfg.ListenAddr != "" {
		args = append(args, "--listen-addr", cfg.ListenAddr)
	}
	if cfg.Port != 0 {
		args = append(args, "--port", strconv.Itoa(cfg.Port))
	}
	if cfg.ServiceName != "" {
		args = append(args, "--service-name", cfg.ServiceName)
	}
	if cfg.Domain != "" {
		args = append(args, "--domain", cfg.Domain)
	}
	if cfg.WebSocketPath != "" {
		args = append(args, "--websocket-path", cfg.WebSocketPath)
	}
	if cfg.ControlEndpoint != "" {
		args = append(args, "--control", cfg.ControlEndpoint)
	}
	return args
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func isControlUnavailable(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) || strings.Contains(strings.ToLower(err.Error()), "pipe")
}

func usageError() error {
	return errors.New("usage: clippy <start|stop|status>")
}
