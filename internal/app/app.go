// Package app wires configuration, service behavior, and the local HTTP server.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"crew-services/internal/config"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
)

// App owns one HTTP server instance.
type App struct {
	server *http.Server

	mu          sync.Mutex
	listen      net.Listener
	finished    chan struct{}
	terminalErr error
	finishOnce  sync.Once
}

var errAlreadyStarted = errors.New("server is already started")

// New constructs the local HTTP server around the supplied service.
func New(cfg config.Config, svc *service.Service) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, errors.New("service is required")
	}
	return &App{
		server: &http.Server{
			Addr:              cfg.ListenAddress,
			Handler:           httpapi.NewHandler(svc),
			ReadHeaderTimeout: 5 * time.Second,
		},
		finished: make(chan struct{}),
	}, nil
}

// Start begins serving and returns after the listener is ready.
func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listen != nil {
		return errAlreadyStarted
	}
	listener, err := net.Listen("tcp", a.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.server.Addr, err)
	}
	a.listen = listener
	go func() {
		err := a.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		a.finish(err)
	}()
	return nil
}

// Address returns the bound listener address after Start.
func (a *App) Address() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listen == nil {
		return ""
	}
	return a.listen.Addr().String()
}

// Run starts the server, waits for context cancellation or a serve failure, and
// shuts it down gracefully.
func (a *App) Run(ctx context.Context) error {
	if err := a.Start(); err != nil && !errors.Is(err, errAlreadyStarted) {
		return err
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.Shutdown(shutdownCtx)
	case <-a.finished:
		return a.terminal()
	}
}

// Shutdown stops a started server cleanly.
func (a *App) Shutdown(ctx context.Context) error {
	if !a.started() {
		return errors.New("server is not started")
	}
	err := a.server.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	return a.Wait(ctx)
}

// Wait observes the terminal result without consuming it, so all callers see
// the same server completion.
func (a *App) Wait(ctx context.Context) error {
	if !a.started() {
		return errors.New("server is not started")
	}
	select {
	case <-a.finished:
		return a.terminal()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listen != nil
}

func (a *App) finish(err error) {
	a.finishOnce.Do(func() {
		a.mu.Lock()
		a.terminalErr = err
		a.mu.Unlock()
		close(a.finished)
	})
}

func (a *App) terminal() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terminalErr
}
