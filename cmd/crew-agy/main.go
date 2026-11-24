// Command crew-agy delivers crew fabric messages into Antigravity CLI (agy)
// conversations through each agy process's own language server, and hands
// them to running turns through agy's PreInvocation hook.
//
//	crew-agy serve [flags]   run the driver (alone, the command prints usage)
//	crew-agy hook EVENT      run as an agy hook (session-start, step, idle, fill-notify)
//	crew-agy send [--policy P] [--as SEAT] --to ADDRESS TEXT...
//	                         send a crew message from this agy conversation
//	crew-agy who             list sessions with crew addresses
//	crew-agy seat COMMAND    take, leave, relieve or read crew seats
//	crew-agy board COMMAND   read and write crew-board threads
//	crew-agy env             print the line that starts agy with the crew token
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
	"strings"
	"syscall"
	"time"

	"crew-services/internal/agydriver"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

const defaultDriverURL = "http://127.0.0.1:8798"

const usage = `usage:
  crew-agy send [--policy P] [--as SEAT] --to ADDRESS TEXT...
  crew-agy who [--all]       list running and recent sessions; --all lists every one
  crew-agy whoami [--person NAME]
  crew-agy seat COMMAND      take, leave, relieve or read crew seats
  crew-agy board COMMAND     read and write crew-board threads
  crew-agy env               print the export line that starts agy with the crew token
  crew-agy hook EVENT        run as an agy hook
  crew-agy serve [flags]     run the driver, which a user unit already does
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		// A hook must never break the conversation: always exit 0.
		hook(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "env" {
		os.Exit(env())
	}
	if len(os.Args) > 1 && (os.Args[1] == "send" || os.Args[1] == "who" || os.Args[1] == "whoami" || os.Args[1] == "seat" || os.Args[1] == "board") {
		// agy gives the commands its agent runs the conversation's id.
		os.Exit(crewcli.Main(context.Background(), os.Args[1:], os.Getenv("ANTIGRAVITY_CONVERSATION_ID"), driverURL(), os.Stdin, os.Stdout, os.Stderr))
	}
	args, code, ok := crewcli.ServeArgs(os.Args[1:], usage, os.Stdout, os.Stderr)
	if !ok {
		os.Exit(code)
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-agy:", err)
		os.Exit(1)
	}
}

func hook(args []string) {
	if len(args) < 1 {
		return
	}
	var opts agydriver.HookOptions
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--den-server=") {
			opts.DenServer = strings.TrimPrefix(a, "--den-server=")
		}
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		return
	}
	var in agydriver.HookInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	// The hook runs (through sh -c) under an agy process or the Antigravity
	// hub; the driver reaches that process's language server.
	proc, _ := agydriver.Procs{}.FindHost(os.Getppid())
	out := agydriver.RunHook(context.Background(), driverURL(), args[0], in, proc, opts)
	if len(out) > 0 {
		_, _ = os.Stdout.Write(out)
	}
}

func driverURL() string {
	if url := os.Getenv("CREW_AGY_URL"); url != "" {
		return url
	}
	return defaultDriverURL
}

func defaultTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config/crew/agy-csrf-token")
}

// env prints the shell line that exports the crew token for agy, so a profile
// can run `eval "$(crew-agy env)"`. It prints nothing when there is no token.
func env() int {
	data, err := os.ReadFile(defaultTokenPath())
	token := strings.TrimSpace(string(data))
	if err != nil || token == "" {
		fmt.Fprintln(os.Stderr, "crew-agy: no token at", defaultTokenPath(), "(scripts/install-crew-agy.sh makes one)")
		return 1
	}
	fmt.Printf("export ANTIGRAVITY_SERVER_CSRF_TOKEN=%s\n", token)
	return 0
}

func serve(args []string) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("crew-agy", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8798", "loopback address for the hook API")
	fabricURL := flags.String("fabric", "http://127.0.0.1:8787", "crew-messaging base URL")
	statePath := flags.String("state", filepath.Join(home, ".local/state/crew-agy/sessions.json"), "driver state file (mode 0600)")
	tokenPath := flags.String("token-file", defaultTokenPath(), "CSRF token agy is started with (ANTIGRAVITY_SERVER_CSRF_TOKEN)")
	adapterID := flags.String("adapter-id", "antigravity", "stable fabric adapter ID")
	interval := flags.Duration("interval", 2*time.Second, "delivery pass interval")
	leaseDuration := flags.Duration("lease-duration", 5*time.Minute, "fabric adapter lease duration")
	instanceID := flags.String("instance-id", "crew-agy-local", "stable fabric adapter instance ID")
	acceptTimeout := flags.Duration("accept-timeout", 2*time.Minute, "how long a posted message may go untaken before it becomes outcome_unknown")
	queuedTimeout := flags.Duration("queued-timeout", 6*time.Hour, "how long a taken message may wait to enter the conversation before it becomes outcome_unknown")
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
	listener, err := crewcli.Listen(*listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	fabric, err := fabricclient.New(*fabricURL)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	driver, err := agydriver.New(fabric, agydriver.LanguageServer{}, agydriver.Procs{}, time.Now, agydriver.Config{AdapterID: *adapterID, InstanceID: *instanceID, StatePath: *statePath, TokenPath: *tokenPath, LeaseDuration: *leaseDuration, AcceptTimeout: *acceptTimeout, QueuedTimeout: *queuedTimeout}, log)
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
	server := &http.Server{Addr: *listen, Handler: agydriver.Handler(driver), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("hook API stopped", "error", err)
			stop()
		}
	}()
	log.Info("crew-agy running", "listen", *listen, "fabric", *fabricURL)
	return driver.Run(ctx, *interval)
}
