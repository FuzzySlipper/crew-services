// playtest-assist runs one bounded controller interval on a caller-owned session.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"crew-services/internal/playtest/assistant"
	"crew-services/internal/playtest/client"
)

type config struct {
	SessionID      string           `json:"session_id"`
	ProductURL     string           `json:"product_url"`
	Commands       []string         `json:"commands"`
	CaptureEvery   int              `json:"capture_every,omitempty"`
	ParentModel    string           `json:"parent_model,omitempty"`
	ParentProtocol string           `json:"parent_protocol,omitempty"`
	Policy         assistant.Policy `json:"policy"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "playtest-assist:", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "interval configuration JSON")
	output := flag.String("output", "", "new interval JSONL transcript")
	serviceURL := flag.String("url", client.DefaultURL, "playtest service URL")
	router := flag.String("router", "http://127.0.0.1:18082", "den-router base URL")
	model := flag.String("model", "jev", "router model alias")
	baseline := flag.Bool("baseline", false, "repeat the first tactic deterministically")
	flag.Parse()
	if *path == "" || *output == "" {
		return fmt.Errorf("--config and --output required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if err = cfg.Policy.Validate(); err != nil {
		return err
	}
	if cfg.SessionID == "" {
		return fmt.Errorf("session_id required")
	}
	service, err := client.New(*serviceURL, nil)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Only the caller-owned session is used; the runner never starts or recovers one.
	status, err := service.Result(ctx, client.Request{Op: "status", SessionID: cfg.SessionID})
	if err != nil {
		return err
	}
	var envelope struct {
		Session struct {
			Phase string `json:"phase"`
			Game  string `json:"game"`
		} `json:"session"`
	}
	if err = json.Unmarshal(status, &envelope); err != nil {
		return err
	}
	st := envelope.Session
	if st.Phase != "connected" {
		return fmt.Errorf("session must be connected: %s", status)
	}
	if cfg.ProductURL != "" {
		game, err := service.Result(ctx, client.Request{Op: "game", Game: st.Game})
		if err != nil {
			return err
		}
		var profile struct {
			URL string `json:"url"`
		}
		if err = json.Unmarshal(game, &profile); err != nil {
			return err
		}
		if profile.URL != cfg.ProductURL {
			return fmt.Errorf("product_url must match session profile URL %q", profile.URL)
		}
	}
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	env := &assistant.SessionEnvironment{Observer: assistant.Observer{Client: service, SessionID: cfg.SessionID, ProductURL: cfg.ProductURL, Commands: cfg.Commands, CaptureEvery: cfg.CaptureEvery}}
	var controller assistant.Controller = &assistant.Jev{BaseURL: *router, Model: *model, Token: os.Getenv("PLAYTEST_ROUTER_TOKEN")}
	if *baseline {
		controller = &assistant.Baseline{}
	}
	var parent assistant.Parent
	if cfg.ParentModel != "" {
		parent = &assistant.HTTPParent{BaseURL: *router, Model: cfg.ParentModel, Protocol: cfg.ParentProtocol, Token: os.Getenv("PLAYTEST_ROUTER_TOKEN")}
	}
	result, runErr := assistant.RunWithParent(ctx, cfg.Policy, env, controller, parent, file)
	if err = file.Sync(); err != nil {
		return err
	}
	json.NewEncoder(os.Stdout).Encode(result)
	return runErr
}
