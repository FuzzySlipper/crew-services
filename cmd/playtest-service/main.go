package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crew-services/internal/playtest/browser"
	"crew-services/internal/playtest/pool"
	"crew-services/internal/playtest/routing"
	"crew-services/internal/playtest/session"
	"crew-services/internal/playtest/wolf"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:48200", "loopback API address")
	poolPath := flag.String("pool", "", "local pool configuration JSON; size and optional queue_wait_ms")
	configPath := flag.String("config", "", "Wolf machine config JSON")
	profilesPath := flag.String("games", "", "game profile JSON array")
	state := flag.String("state", "", "durable session/script state directory")
	worker := flag.String("worker", "", "Node script worker path")
	forward := flag.String("forward", "", "Linux container forwarder binary path")
	browserWorker := flag.String("browser-worker", "", "optional Playwright browser worker path")
	chromium := flag.String("chromium", "", "optional Chromium executable for browser backend")
	flag.Parse()
	if *profilesPath == "" || *state == "" || *worker == "" || (*configPath == "" && *browserWorker == "") {
		log.Fatal("--games, --state, --worker and at least one of --config (Wolf) or --browser-worker are required")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be a loopback IP address")
	}
	var config wolf.Config
	if *configPath != "" {
		readJSON(*configPath, &config)
	}
	var profiles []session.Profile
	readJSON(*profilesPath, &profiles)
	if len(profiles) == 0 {
		log.Fatal("at least one game profile is required")
	}
	paths := []string{*worker}
	if *configPath != "" {
		paths = append(paths, *forward)
	}
	for _, path := range paths {
		if _, err = os.Stat(path); err != nil {
			log.Fatal(err)
		}
	}
	absoluteState, err := filepath.Abs(*state)
	if err != nil {
		log.Fatal(err)
	}
	type poolConfig struct {
		Size        int  `json:"size"`
		QueueWaitMS *int `json:"queue_wait_ms,omitempty"`
	}
	pc := poolConfig{Size: 1}
	queueWait := 15 * time.Second
	if *poolPath != "" {
		readJSON(*poolPath, &pc)
		if pc.Size < 1 {
			log.Fatal("pool size must be positive")
		}
		if pc.QueueWaitMS != nil {
			queueWait = time.Duration(*pc.QueueWaitMS) * time.Millisecond
		}
	}
	var services []*session.Service
	usedTargets, usedCaptures, usedMoonlight := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for index := 0; index < pc.Size; index++ {
		slotState := absoluteState
		if index > 0 {
			slotState = filepath.Join(absoluteState, "slots", fmt.Sprintf("slot-%d", index+1))
		}
		slotConfig := config
		if *poolPath != "" && *configPath != "" {
			readJSON(filepath.Join(filepath.Dir(*poolPath), "slots", fmt.Sprintf("slot-%d", index+1), "machine.json"), &slotConfig)
		}
		router := &routing.Router{Entries: map[string]routing.Entry{}}
		if *configPath != "" {
			targetKey := fmt.Sprintf("%s:%d", slotConfig.SSHHost, slotConfig.TargetPort)
			captureKey, _ := filepath.Abs(slotConfig.State)
			moonlightKey, _ := filepath.Abs(slotConfig.MoonlightConfig)
			if usedTargets[targetKey] || usedCaptures[captureKey] || usedMoonlight[moonlightKey] {
				log.Fatal("pool slots must have distinct native targets, capture directories and Moonlight configurations")
			}
			usedTargets[targetKey] = true
			usedCaptures[captureKey] = true
			usedMoonlight[moonlightKey] = true
			backend, err := wolf.New(slotConfig)
			if err != nil {
				log.Fatal(err)
			}
			defer backend.Close()
			launcher := &session.WolfLauncher{Backend: backend, SSHHost: slotConfig.SSHHost, ForwardBinary: *forward}
			router.Entries["wolf"] = routing.Entry{Backend: backend, Launcher: launcher}
		}
		if *browserWorker != "" {
			backend, err := browser.New(browser.Config{State: filepath.Join(slotState, "browser"), Worker: *browserWorker, Chromium: *chromium})
			if err != nil {
				log.Fatal(err)
			}
			defer backend.Close()
			router.Entries["browser"] = routing.Entry{Backend: backend, Launcher: backend}
		}
		service, err := session.New(router, router, profiles, slotState, *worker)
		if err != nil {
			log.Fatal(err)
		}
		services = append(services, service)
	}
	var service interface {
		session.Commander
		Close(context.Context) error
	} = services[0]
	if *poolPath != "" {
		pooled, err := pool.New(services, queueWait)
		if err != nil {
			log.Fatal(err)
		}
		service = pooled
	}
	server := &http.Server{Addr: *listen, Handler: session.CommandHandler(service), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := service.Close(cleanup); err != nil {
			log.Printf("session cleanup: %v", err)
		}
		_ = server.Shutdown(cleanup)
	}()
	log.Printf("playtest API listening on %s", *listen)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func readJSON(path string, value any) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	if err = json.Unmarshal(data, value); err != nil {
		log.Fatal(err)
	}
}
