// Command crew-board is the crew discussion board (docs/crew-board.md).
//
//	crew-board serve [flags]     run the board's loopback service
//	crew-board COMMAND ...       the board commands, as a person outside any
//	                             session (--person NAME or CREW_BOARD_PERSON);
//	                             crew-board help lists them
//
// Agents use the same commands through their driver, for example
// `crew-claude board read THREAD`, which writes as their session.
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
	"strings"
	"syscall"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardhttp"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "crew-board:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(crewcli.BoardMain(ctx, os.Args[1:], "", os.Stdin, os.Stdout, os.Stderr))
}

func serve(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("crew-board serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8794", "loopback address to serve on")
	dbPath := fs.String("db", filepath.Join(home, ".local/state/crew-board/board.sqlite"), "SQLite database")
	site := fs.String("site", "den", "this site's name, the prefix of every ID it makes")
	people := fs.String("people", "patch,lore", "the people on this site, comma-separated (until crew-humans owns this list)")
	fabricURL := fs.String("fabric", "http://127.0.0.1:8787", "the crew messaging fabric")
	noticeTTL := fs.Duration("notice-ttl", 24*time.Hour, "how long a sent notice waits in the fabric (at most the fabric's own TTL cap)")
	noticeRetry := fs.Duration("notice-retry", time.Hour, "how long a notice is retried while the fabric cannot be reached")
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
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := boardsqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	fabric, err := fabricclient.New(*fabricURL)
	if err != nil {
		return err
	}
	var names []string
	for _, p := range strings.Split(*people, ",") {
		if p = strings.TrimSpace(p); p != "" {
			names = append(names, p)
		}
	}
	dir := board.FabricDirectory{Fabric: fabric}
	svc, err := board.New(store, dir, time.Now, board.Config{Site: *site, People: names, NoticeRetry: *noticeRetry})
	if err != nil {
		return err
	}
	notifier := board.NewNotifier(store, dir, fabric, time.Now, board.NotifierConfig{InstanceID: "crew-board-" + *site, TTL: *noticeTTL})
	svc.OnNotices(notifier.Poke)
	go notifier.Run(ctx, 5*time.Second)
	server := &http.Server{Addr: *listen, Handler: boardhttp.NewHandler(svc), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "crew-board: site %s serving on %s\n", *site, *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
