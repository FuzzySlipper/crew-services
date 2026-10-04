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

	"crew-services/internal/devserver"
	"crew-services/internal/playtest/hosting"
	"crew-services/internal/playtest/pool"
	"crew-services/internal/playtest/session"
	"crew-services/internal/serve"
)

// reaperInterval is how often sessions are checked for idle expiry and for a
// product host that ended by itself.
const reaperInterval = 30 * time.Second

// defaultRetireDir holds what retention pruning removes until pruning has
// been checked over a few rounds; then pass --retire-dir "" to delete.
const defaultRetireDir = "/data/crew-playtest-pending-delete"

func main() {
	listen := flag.String("listen", "127.0.0.1:48200", "loopback API address")
	poolPath := flag.String("pool", "", "local pool configuration JSON; size and optional queue_wait_ms")
	profilesPath := flag.String("games", "", "game profile JSON array")
	state := flag.String("state", "", "durable session/script state directory")
	worker := flag.String("worker", "", "Node script worker path")
	browserWorker := flag.String("browser-worker", "", "Playwright browser worker path")
	chromium := flag.String("chromium", "", "optional Chromium executable for browser backend")
	serveConfig := flag.String("serve-config", "", "den-serve configuration for session-owned product hosts; default shares den-serve's state")
	retireDir := flag.String("retire-dir", defaultRetireDir, "where pruned session records and evidence are moved instead of deleted; empty deletes them")
	rustyPath := flag.String("rusty", "", "rusty CLI used to explain hosts that ended by themselves; default from PATH or ~/.local/bin")
	flag.Parse()
	if *profilesPath == "" || *state == "" || *worker == "" || *browserWorker == "" {
		log.Fatal("--games, --state, --worker and --browser-worker are required")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be a loopback IP address")
	}
	profiles, pc, err := loadConfiguration(*profilesPath, *poolPath)
	if err != nil {
		log.Fatal(err)
	}
	for _, path := range []string{*worker, *browserWorker} {
		if _, err = os.Stat(path); err != nil {
			log.Fatal(err)
		}
	}
	absoluteState, err := filepath.Abs(*state)
	if err != nil {
		log.Fatal(err)
	}
	registry := session.NewRegistry(profiles)
	hostConfig, err := serve.DefaultConfig()
	if *serveConfig != "" {
		hostConfig, err = serve.LoadConfigFromPath(*serveConfig)
	}
	if err != nil {
		log.Fatal(err)
	}
	hosts, err := devserver.NewManager(hostConfig.Manager)
	if err != nil {
		log.Fatal(err)
	}
	builder := slotBuilder{state: absoluteState, worker: *worker, browserWorker: *browserWorker, chromium: *chromium, registry: registry,
		hosts: hosts, manifest: hosting.ManifestProject(hostConfig.Manager), locks: &hosting.RepoLocks{}, retireDir: *retireDir}
	if *rustyPath == "" {
		*rustyPath = hosting.FindRusty()
	}
	if *rustyPath != "" {
		builder.ended = hosting.RustyDevEndReason(*rustyPath)
	}
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
	if err := service.SetLifecycle(pc.lifecycle()); err != nil {
		log.Fatal(err)
	}
	service.RunReaper(reaperInterval, log.Printf)
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
