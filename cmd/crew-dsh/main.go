// Command crew-dsh delivers crew fabric messages into DeepSeek Harness (DSH)
// sessions through the `dsh web` host's session API. It never starts, stops
// or restarts DSH; the host resumes a cold session itself when prompted.
//
//	crew-dsh [serve] [flags]   run the driver
//	crew-dsh send [--policy P] --to ADDRESS TEXT...
//	                           send a crew message from this DSH session
//	crew-dsh who               list the sessions with crew addresses
//	crew-dsh address           print this DSH session's crew address
//
// send and address read the session from DSH_SESSION_ID, which DSH sets for
// the commands its agents run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"crew-services/internal/crewcli"
	"crew-services/internal/dshdriver"
	"crew-services/internal/fabricclient"
)

const defaultDriverURL = "http://127.0.0.1:8793"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "send", "who":
			os.Exit(crewcli.Main(context.Background(), os.Args[1:], os.Getenv("DSH_SESSION_ID"), driverURL(), os.Stdin, os.Stdout, os.Stderr))
		case "address":
			os.Exit(address())
		}
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil {
		fmt.Fprintln(os.Stderr, "crew-dsh:", err)
		os.Exit(1)
	}
}

func driverURL() string {
	if url := os.Getenv("CREW_DSH_URL"); url != "" {
		return url
	}
	return defaultDriverURL
}

func address() int {
	session := os.Getenv("DSH_SESSION_ID")
	if session == "" {
		fmt.Fprintln(os.Stderr, "not running inside a DSH session (DSH_SESSION_ID is not set)")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, driverURL()+"/v1/sessions/"+url.PathEscape(session)+"/address", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crew-dsh driver:", err)
		return 1
	}
	defer resp.Body.Close()
	var body struct {
		Address string `json:"address"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Address == "" {
		fmt.Fprintln(os.Stderr, "no crew address:", body.Error)
		return 1
	}
	fmt.Println(body.Address)
	return 0
}

func serve(args []string) error {
	cfg, err := dshdriver.Parse(args)
	if err != nil {
		return err
	}
	fabric, err := fabricclient.New(cfg.FabricURL)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	host := dshdriver.NewWebHost(dshdriver.JournalTokens(cfg.DSHUnit, cfg.DSHURL))
	driver := dshdriver.NewDriver(fabric, host, time.Now, cfg, log)
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
	server := &http.Server{Addr: cfg.ControlListen, Handler: dshdriver.Handler(driver), ReadHeaderTimeout: 5 * time.Second}
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
	log.Info("crew-dsh running", "listen", cfg.ControlListen, "fabric", cfg.FabricURL, "dsh_unit", cfg.DSHUnit)
	return driver.Run(ctx, cfg.PollInterval)
}
