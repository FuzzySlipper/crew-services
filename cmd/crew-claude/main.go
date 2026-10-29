// Command crew-claude delivers crew fabric messages into Claude Code sessions
// through each session's own inbox socket.
//
//	crew-claude serve [flags]   run the driver
//	crew-claude hook EVENT      run as a Claude Code hook (session-start, busy, idle, fill-notify)
//	crew-claude send [--policy P] --to ADDRESS TEXT...
//	                            send a crew message from this Claude session
//	crew-claude who             list Claude and Codex sessions with crew addresses
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crew-services/internal/claudedriver"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

const defaultDriverURL = "http://127.0.0.1:8792"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		// A hook must never break the session: always exit 0.
		hook(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "send" || os.Args[1] == "who") {
		os.Exit(crewcli.Main(context.Background(), os.Args[1:], os.Getenv("CLAUDE_CODE_SESSION_ID"), driverURL(), os.Stdin, os.Stdout, os.Stderr))
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-claude:", err)
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
	var in claudedriver.HookInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	out := claudedriver.RunHook(context.Background(), driverURL(), args[0], in, claudedriver.HookEnv{Socket: os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), Token: os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")})
	if len(out) > 0 {
		_, _ = os.Stdout.Write(out)
	}
}

func driverURL() string {
	if url := os.Getenv("CREW_CLAUDE_URL"); url != "" {
		return url
	}
	return defaultDriverURL
}

func serve(args []string) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("crew-claude", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8792", "loopback address for the hook API")
	fabricURL := flags.String("fabric", "http://127.0.0.1:8787", "crew-messaging base URL")
	statePath := flags.String("state", filepath.Join(home, ".local/state/crew-claude/sessions.json"), "driver state file (socket paths and tokens; mode 0600)")
	adapterID := flags.String("adapter-id", "claude-code", "stable fabric adapter ID")
	interval := flags.Duration("interval", 2*time.Second, "delivery pass interval")
	leaseDuration := flags.Duration("lease-duration", 5*time.Minute, "fabric adapter lease duration")
	// A stable instance ID lets a restarted driver take over its own lease
	// instead of waiting for the previous process's lease to expire.
	instanceID := flags.String("instance-id", "crew-claude-local", "stable fabric adapter instance ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid -listen: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("-listen must be a loopback address")
	}
	fabric, err := fabricclient.New(*fabricURL)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	driver, err := claudedriver.New(fabric, claudedriver.SocketInbox{}, time.Now, claudedriver.Config{AdapterID: *adapterID, InstanceID: *instanceID, StatePath: *statePath, LeaseDuration: *leaseDuration}, log)
	if err != nil {
		return err
	}
	driver.Lookup = crewcli.Lookup
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
	server := &http.Server{Addr: *listen, Handler: claudedriver.Handler(driver), ReadHeaderTimeout: 5 * time.Second}
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
	log.Info("crew-claude running", "listen", *listen, "fabric", *fabricURL)
	return driver.Run(ctx, *interval)
}
