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
	"runtime"
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

const usage = `usage:
  crew-hermes send [--policy P] [--as SEAT] --to ADDRESS TEXT...
  crew-hermes who [--all]       list running and recent sessions; --all lists every one
  crew-hermes whoami [--person NAME]
  crew-hermes seat COMMAND      take, leave, relieve or read crew seats
  crew-hermes board COMMAND     read and write crew-board threads
  crew-hermes address           print this profile's crew address
  crew-hermes install           write the delivery helper and each profile's crew plugin
  crew-hermes serve [flags]     run the driver, which a user unit already does
`

func main() {
	loadEnvDefaults()
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
	args, code, ok := crewcli.ServeArgs(os.Args[1:], usage, os.Stdout, os.Stderr)
	if !ok {
		os.Exit(code)
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-hermes:", err)
		os.Exit(1)
	}
}

// loadEnvDefaults reads ~/.config/crew/env, KEY=VALUE lines, for variables
// not already set. A Hermes host with its own fabric (not reached through a
// tunnel) names it there, since the gateway's agents inherit only the
// gateway's environment.
func loadEnvDefaults() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "crew", "env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok {
			setDefault(strings.TrimSpace(name), strings.Trim(strings.TrimSpace(value), `"'`))
		}
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
	if runtime.GOOS == "windows" {
		// Hermes for Windows keeps its home in the local app data folder.
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "hermes")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hermes")
}

// adapterID names this driver to the fabric. A profile's address derives from
// it, so a second Hermes host needs its own (CREW_HERMES_ADAPTER), or its
// profiles would take the first host's addresses.
func adapterID() string {
	if id := strings.TrimSpace(os.Getenv("CREW_HERMES_ADAPTER")); id != "" {
		return id
	}
	return "crew-hermes"
}

// hermesPython finds the newest Python bundled with Hermes.
func hermesPython() (string, error) {
	pattern := filepath.Join(hermesRoot(), "tools", "python-*", "bin", "python3")
	if runtime.GOOS == "windows" {
		pattern = filepath.Join(hermesRoot(), "tools", "python-*", "python.exe")
	}
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return "", fmt.Errorf("no Hermes Python under %s; pass -python", filepath.Join(hermesRoot(), "tools"))
	}
	return matches[len(matches)-1], nil
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
	cfg.AdapterID = adapterID()
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	cfg.InstanceID = cfg.AdapterID + "-" + user + "@" + host
	cfg.ControlListen = strings.TrimPrefix(driverURL(), "http://")
	cfg.ReplyCommand, cfg.Runtime = "crew-hermes", "hermes"
	cfg.Recent = 100 * 365 * 24 * time.Hour
	var python, agent string
	var err error
	flags := flag.NewFlagSet("crew-hermes", flag.ContinueOnError)
	flags.StringVar(&cfg.FabricURL, "fabric-url", cfg.FabricURL, "crew-messaging base URL (the SSH tunnel's local end)")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "delivery pass interval")
	flags.StringVar(&python, "python", "", "Hermes's Python (default: the newest under the Hermes home's tools)")
	flags.StringVar(&agent, "agent", filepath.Join(hermesRoot(), "hermes-agent"), "the hermes-agent checkout")
	if err = flags.Parse(args); err != nil {
		return err
	}
	if python == "" {
		if python, err = hermesPython(); err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	listener, err := crewcli.Listen(cfg.ControlListen)
	if err != nil {
		return err
	}
	defer listener.Close()
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
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
			text += crewcli.BoardContext("crew-hermes") + crewcli.SeatContext("crew-hermes", seats[s.Address]) + crewcli.PersonContext(crewcli.PersonFor(s.NativeID))
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
