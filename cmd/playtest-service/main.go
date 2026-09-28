package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crew-services/internal/playtest/pool"
	"crew-services/internal/playtest/session"
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
	profiles, pc, err := loadConfiguration(*profilesPath, *poolPath)
	if err != nil {
		log.Fatal(err)
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
	registry := session.NewRegistry(profiles)
	configs, err := loadWolfConfigs(*configPath, *poolPath)
	if err != nil {
		log.Fatal(err)
	}
	builder := slotBuilder{state: absoluteState, worker: *worker, browserWorker: *browserWorker, chromium: *chromium, forward: *forward, wolfEnabled: *configPath != "", wolfConfigs: configs, registry: registry}
	var services []*session.Service
	for index := 0; index < pc.Size; index++ {
		slot, release, err := builder.create(index)
		if err != nil {
			log.Fatal(err)
		}
		defer release()
		services = append(services, slot)
	}
	service, err := pool.NewResizable(services, pc.wait(), registry, builder.create)
	if err != nil {
		log.Fatal(err)
	}
	commands := &reloadService{pool: service, gamesPath: *profilesPath, poolPath: *poolPath}
	if *poolPath == "" {
		commands.single = services[0]
	}
	server := &http.Server{Addr: *listen, Handler: session.CommandHandler(commands), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
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
