package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/config"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

func TestStartServesHealthAndShutsDown(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	svc, err := service.New(store, service.SystemClock{})
	if err != nil {
		t.Fatalf("New service error = %v", err)
	}
	application, err := New(config.Config{
		ListenAddress: testListenAddress(t),
		SQLitePath:    "state.db",
		LeaseDuration: time.Minute,
		TTLDuration:   time.Hour,
		Retention:     time.Hour,
	}, svc)
	if err != nil {
		t.Fatalf("New app error = %v", err)
	}
	if err := application.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	response, err := http.Get("http://" + application.Address() + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("health response = %d %q", response.StatusCode, body)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := application.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestConcurrentShutdownAndRunObserveOneTerminalResult(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	svc, err := service.New(store, service.SystemClock{})
	if err != nil {
		t.Fatalf("New service error = %v", err)
	}
	application, err := New(config.Config{
		ListenAddress: testListenAddress(t),
		SQLitePath:    "state.db",
		LeaseDuration: time.Minute,
		TTLDuration:   time.Hour,
		Retention:     time.Hour,
	}, svc)
	if err != nil {
		t.Fatalf("New app error = %v", err)
	}
	if err := application.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	results := make(chan error, 3)
	go func() { results <- application.Run(context.Background()) }()
	for range 2 {
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- application.Shutdown(shutdownCtx)
		}()
	}

	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("lifecycle result = %v, want nil", err)
			}
		case <-time.After(time.Second):
			t.Fatal("lifecycle caller did not observe terminal result")
		}
	}
}

func testListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}
	return address
}
