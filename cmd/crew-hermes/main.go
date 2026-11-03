// Command crew-hermes delivers crew fabric messages into Hermes, where each
// profile is one crew address (docs/hermes.md). It runs on the Hermes host as
// the Hermes user, and reaches the fabric and crew-board on den-agents through
// an SSH tunnel that may only forward to them.
//
//	crew-hermes serve [flags]    run the driver
//	crew-hermes install          write the delivery helper and each profile's crew plugin
//	crew-hermes send|who|seat|board ...
//	                             the crew commands, as this Hermes profile
//	crew-hermes address          print this profile's crew address
//
// Inside a Hermes agent the profile is HERMES_SESSION_PROFILE (or the last
// part of HERMES_HOME); outside one there is no session, as at any terminal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/dshdriver"
	"crew-services/internal/fabricclient"
	"crew-services/internal/hermesdriver"
)

const (
	defaultDriverURL = "http://127.0.0.1:8796"
	// The SSH tunnel's local ends of the fabric and crew-board.
	defaultFabricURL = "http://127.0.0.1:18787"
	defaultBoardURL  = "http://127.0.0.1:18794"
)

func main() {
	setDefault("CREW_FABRIC_URL", defaultFabricURL)
	setDefault("CREW_BOARD_URL", defaultBoardURL)
	crewcli.DriverURLs = map[string]string{"hermes": driverURL()}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "send", "who", "whoami", "seat", "board":
			os.Exit(crewcli.Main(context.Background(), os.Args[1:], profile(), driverURL(), os.Stdin, os.Stdout, os.Stderr))
		case "address":
			os.Exit(address())
		case "install":
			os.Exit(install())
		}
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-hermes:", err)
		os.Exit(1)
	}
}

func setDefault(name, value string) {
	if os.Getenv(name) == "" {
		_ = os.Setenv(name, value)
	}
}

func driverURL() string {
	if url := os.Getenv("CREW_HERMES_URL"); url != "" {
		return url
	}
	return defaultDriverURL
}

// profile is the Hermes profile this command runs in, or "" outside a Hermes
// agent.
func profile() string {
	if os.Getenv("HERMES_AGENT") == "" {
		return ""
	}
	if p := os.Getenv("HERMES_SESSION_PROFILE"); p != "" {
		return p
	}
	home := os.Getenv("HERMES_HOME")
	if filepath.Base(filepath.Dir(home)) == "profiles" {
		return filepath.Base(home)
	}
	return hermesdriver.DefaultProfile
}

func hermesRoot() string {
	if root := os.Getenv("CREW_HERMES_ROOT"); root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hermes")
}

func stateDir() string {
	if dir := os.Getenv("CREW_HERMES_STATE"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".crew-hermes")
}

func address() int {
	p := profile()
	if p == "" {
		fmt.Fprintln(os.Stderr, "not running inside a Hermes agent (HERMES_AGENT is not set)")
		return 1
	}
	for _, s := range func() []crewcli.SessionInfo { s, _ := crewcli.Who(context.Background(), crewcli.DriverURLs); return s }() {
		if s.NativeID == p {
			fmt.Println(s.Address)
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "no crew address for profile", p)
	return 1
}

func install() int {
	h := &hermesdriver.Hermes{Root: hermesRoot(), Dir: stateDir()}
	profiles, err := h.Install()
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Printf("installed the delivery helper in %s and the crew plugin in profiles %s\n", h.Dir, strings.Join(profiles, ", "))
	fmt.Println("enable the plugin in each profile with: hermes -p PROFILE plugins enable crew")
	return 0
}

func serve(args []string) error {
	cfg := dshdriver.Defaults()
	cfg.FabricURL = os.Getenv("CREW_FABRIC_URL")
	cfg.AdapterID = "crew-hermes"
	host, _ := os.Hostname()
	cfg.InstanceID = "crew-hermes-" + os.Getenv("USER") + "@" + host
	cfg.ControlListen = strings.TrimPrefix(driverURL(), "http://")
	cfg.ReplyCommand, cfg.Runtime = "crew-hermes", "hermes"
	cfg.Recent = 100 * 365 * 24 * time.Hour
	var python, agent string
	flags := flag.NewFlagSet("crew-hermes", flag.ContinueOnError)
	flags.StringVar(&cfg.FabricURL, "fabric-url", cfg.FabricURL, "crew-messaging base URL (the SSH tunnel's local end)")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "delivery pass interval")
	flags.StringVar(&python, "python", "", "Hermes's Python (default: the newest under ~/.hermes/tools)")
	flags.StringVar(&agent, "agent", filepath.Join(hermesRoot(), "hermes-agent"), "the hermes-agent checkout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if python == "" {
		matches, _ := filepath.Glob(filepath.Join(hermesRoot(), "tools", "python-*", "bin", "python3"))
		if len(matches) == 0 {
			return errors.New("no Hermes Python under ~/.hermes/tools; pass -python")
		}
		python = matches[len(matches)-1]
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	fabric, err := fabricclient.New(cfg.FabricURL)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hermes, err := hermesdriver.NewHermes(hermesRoot(), stateDir(), python, agent, time.Now)
	if err != nil {
		return err
	}
	if _, err := hermes.Install(); err != nil {
		return fmt.Errorf("install the helper and plugins: %w", err)
	}
	driver := dshdriver.NewDriver(fabric, hermes, time.Now, cfg, log)
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
	mux := http.NewServeMux()
	// A profile's posts carry its model and, as their label, the profile.
	mux.HandleFunc("GET /v1/sessions/{sessionID}/model", crewcli.ServeAbout(func(r *http.Request, id string) crewcli.About {
		return crewcli.About{Model: driver.Model(r.Context(), id), Label: id}
	}))
	mux.Handle("/", dshdriver.Handler(driver))
	server := &http.Server{Addr: cfg.ControlListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control API stopped", "error", err)
			stop()
		}
	}()
	go keepIntros(ctx, driver, hermes, cfg.FabricURL, log)
	log.Info("crew-hermes running", "listen", cfg.ControlListen, "fabric", cfg.FabricURL, "hermes", hermesRoot(), "python", python)
	return driver.Run(ctx, cfg.PollInterval)
}

// keepIntros keeps each profile's intro (address, seats, commands) current
// for the crew plugin.
func keepIntros(ctx context.Context, driver *dshdriver.Driver, hermes *hermesdriver.Hermes, fabricURL string, log *slog.Logger) {
	for {
		seats := crewcli.SeatsByHolder(ctx, fabricURL)
		for _, s := range driver.Sessions() {
			text := fmt.Sprintf("Your crew messaging address is %s (Hermes profile %s). Crew services and other agents send messages here; reply or write with `crew-hermes send --to ADDRESS TEXT`.", s.Address, s.NativeID)
			text += crewcli.BoardContext("crew-hermes") + crewcli.SeatContext("crew-hermes", seats[s.Address]) + crewcli.PersonContext(crewcli.PersonFor(""))
			if err := hermes.WriteIntro(s.NativeID, text); err != nil {
				log.Warn("write profile intro", "profile", s.NativeID, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
