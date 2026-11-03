// Command crew-humans is the crew service for the people on a site and how
// each is notified (docs/crew-humans.md).
//
//	crew-humans serve [flags]    run the loopback service
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crew-services/internal/humans"
	"crew-services/internal/humans/humanshttp"
	"crew-services/internal/humans/humanssqlite"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: crew-humans serve [-listen ADDR] [-db PATH] [-people FILE]")
		os.Exit(2)
	}
	if err := serve(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "crew-humans:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("crew-humans serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8797", "loopback address to serve on")
	dbPath := fs.String("db", filepath.Join(home, ".local/state/crew-humans/humans.sqlite"), "SQLite database")
	peoplePath := fs.String("people", filepath.Join(home, ".config/crew-humans/people.json"), "the people file")
	giveUp := fs.Duration("give-up", time.Hour, "how long a push is retried while its route cannot be reached")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("listen must be a loopback address")
	}
	people, err := humans.LoadPeople(*peoplePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := humanssqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	svc, err := humans.New(store, time.Now, humans.Config{People: people, GiveUp: *giveUp})
	if err != nil {
		return err
	}
	deliverer := humans.NewDeliverer(svc, store, map[string]humans.Pusher{humans.RouteHomeAssistant: humans.HomeAssistant{}}, time.Now)
	svc.OnNotice(deliverer.Poke)
	go deliverer.Run(ctx, 5*time.Second)
	server := &http.Server{Addr: *listen, Handler: humanshttp.NewHandler(svc), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "crew-humans: site %s, %d people, serving on %s\n", people.Site, len(people.People), *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
