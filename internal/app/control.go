package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sync"
)

const (
	controlCommandStatus = "status"
	controlCommandStop   = "stop"
)

type controlRequest struct {
	Command string `json:"command"`
}

type controlResponse struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// ControlHandler handles local control-plane requests.
type ControlHandler interface {
	Status() Status
	Stop()
}

// ControlServer serves local daemon control requests.
type ControlServer struct {
	logger    *log.Logger
	listener  net.Listener
	closeFn   func() error
	handler   ControlHandler
	closeOnce sync.Once
}

// NewControlServer creates the platform-native control listener.
func NewControlServer(endpoint string, handler ControlHandler, logger *log.Logger) (*ControlServer, error) {
	listener, closeFn, err := listenControl(endpoint)
	if err != nil {
		return nil, err
	}

	return &ControlServer{
		logger:   logger,
		listener: listener,
		closeFn:  closeFn,
		handler:  handler,
	}, nil
}

// Serve accepts control connections until the context is canceled or Close is called.
func (s *ControlServer) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}

		go s.handleConn(conn)
	}
}

// Close stops the control listener.
func (s *ControlServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.Close()
		if s.closeFn != nil {
			if cerr := s.closeFn(); err == nil {
				err = cerr
			}
		}
	})
	return err
}

func (s *ControlServer) handleConn(conn net.Conn) {
	defer conn.Close()

	var req controlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: err.Error()})
		return
	}

	switch req.Command {
	case controlCommandStatus:
		_ = json.NewEncoder(conn).Encode(controlResponse{
			OK:     true,
			Status: ptrStatus(s.handler.Status()),
		})
	case controlCommandStop:
		_ = json.NewEncoder(conn).Encode(controlResponse{
			OK:     true,
			Status: ptrStatus(s.handler.Status()),
		})
		s.handler.Stop()
	default:
		_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: "unknown command"})
	}
}

// RequestControl sends one control request to a running daemon.
func RequestControl(ctx context.Context, endpoint, command string) (*Status, error) {
	conn, err := dialControl(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(controlRequest{Command: command}); err != nil {
		return nil, err
	}

	var resp controlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	if resp.Status == nil {
		return nil, nil
	}
	return resp.Status, nil
}

func ptrStatus(status Status) *Status {
	return &status
}
