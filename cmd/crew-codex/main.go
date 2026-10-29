// Command crew-codex delivers crew fabric messages into threads of the shared
// Codex app-server daemon. It connects to the daemon's control socket and
// never starts, stops or restarts Codex or its threads.
//
//	crew-codex [serve] [flags]   run the driver
//	crew-codex hook EVENT        run as a Codex hook (session-start, fill-notify)
//	crew-codex send [--policy P] --to ADDRESS TEXT...
//	                             send a crew message from this Codex thread
//	crew-codex who               list Claude and Codex sessions with crew addresses
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"crew-services/internal/codexadapter"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

const defaultDriverURL = "http://127.0.0.1:8788"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		// A hook must never break the thread: always exit 0.
		hook(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "send" || os.Args[1] == "who") {
		os.Exit(crewcli.Main(context.Background(), os.Args[1:], os.Getenv("CODEX_THREAD_ID"), driverURL(), os.Stdin, os.Stdout, os.Stderr))
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-codex:", err)
		os.Exit(1)
	}
}

func hook(args []string) {
	if len(args) != 1 {
		return
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in codexadapter.HookInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	if out := codexadapter.RunHook(context.Background(), driverURL(), args[0], in); len(out) > 0 {
		_, _ = os.Stdout.Write(out)
	}
}

func driverURL() string {
	if url := os.Getenv("CREW_CODEX_URL"); url != "" {
		return url
	}
	return defaultDriverURL
}

func serve(args []string) error {
	cfg, err := codexadapter.Parse(args)
	if err != nil {
		return err
	}
	fabric, err := fabricclient.New(cfg.FabricURL)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	driver, err := codexadapter.NewDriver(fabric, time.Now, cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if err = driver.Start(ctx); err == nil {
			break
		}
		log.Warn("fabric not reachable; retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	server := &http.Server{Addr: cfg.ControlListen, Handler: codexadapter.Handler(driver), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("hook API stopped", "error", err)
			stop()
		}
	}()
	log.Info("crew-codex running", "listen", cfg.ControlListen, "fabric", cfg.FabricURL, "daemon", cfg.DaemonSocket)
	return driver.Run(ctx, cfg.PollInterval, func(ctx context.Context) (codexadapter.Connection, error) {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return codexadapter.DialDaemon(dialCtx, cfg.DaemonSocket)
	})
}
