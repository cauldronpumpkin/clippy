//go:build !windows

package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"
)

type fakeControlHandler struct {
	status Status
	stopCh chan struct{}
}

func (f *fakeControlHandler) Status() Status {
	return f.status
}

func (f *fakeControlHandler) Stop() {
	close(f.stopCh)
}

func TestControlServerStatusAndStop(t *testing.T) {
	t.Parallel()

	endpoint := fmt.Sprintf("%s/clippy-test-%d.sock", os.TempDir(), time.Now().UnixNano())
	handler := &fakeControlHandler{
		status: Status{Running: true, Name: "node"},
		stopCh: make(chan struct{}),
	}

	server, err := NewControlServer(endpoint, handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewControlServer() error = %v", err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = server.Serve(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var status *Status
	for time.Now().Before(deadline) {
		status, err = RequestControl(context.Background(), endpoint, controlCommandStatus)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("RequestControl(status) error = %v", err)
	}
	if status == nil || !status.Running || status.Name != "node" {
		t.Fatalf("unexpected status response: %#v", status)
	}

	if _, err := RequestControl(context.Background(), endpoint, controlCommandStop); err != nil {
		t.Fatalf("RequestControl(stop) error = %v", err)
	}

	select {
	case <-handler.stopCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected stop handler to be invoked")
	}
}
