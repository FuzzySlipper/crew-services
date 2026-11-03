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
	"log/slog"
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
	"crew-services/internal/board/boardweb"
	"crew-services/internal/board/network"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
	"crew-services/internal/humansclient"
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
	people := fs.String("people", "patch,lore", "the people on this site, comma-separated, used when -humans is empty or crew-humans cannot be reached at start")
	humansURL := fs.String("humans", "", "crew-humans, which lists this site's people and notifies them (for example http://127.0.0.1:8797); empty records notices to people as no_route")
	webURL := fs.String("web-url", "", "the people's pages as a phone reaches them (for example https://board.dragonden.stream), linked from notices to people")
	fabricURL := fs.String("fabric", "http://127.0.0.1:8787", "the crew messaging fabric")
	noticeTTL := fs.Duration("notice-ttl", 24*time.Hour, "how long a sent notice waits in the fabric (at most the fabric's own TTL cap)")
	noticeRetry := fs.Duration("notice-retry", time.Hour, "how long a notice is retried while the fabric cannot be reached")
	mediaDir := fs.String("media", filepath.Join(home, ".local/state/crew-board/media"), "where attachment blobs, held originals and scratch files live")
	scanner := fs.String("scanner", "", "signature scan command run on each attachment, file path appended (for example \"clamscan --no-summary\"); empty skips the scan")
	noSandbox := fs.Bool("no-sandbox", false, "run decoders without bubblewrap (only for a box without it)")
	networksFile := fs.String("networks", filepath.Join(home, ".config/crew-board/networks.json"), "the networks this site shares boards on (a missing file means none)")
	pullEvery := fs.Duration("pull-every", time.Minute, "how often each network is pulled")
	webListen := fs.String("web-listen", "", "address for the people's pages, for example 0.0.0.0:8795 (they answer only the LAN, Tailscale and loopback); empty serves none")
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
	var networks []network.Config
	if _, err := os.Stat(*networksFile); err == nil {
		if networks, err = network.LoadConfigs(*networksFile, home, filepath.Join(filepath.Dir(*dbPath), "networks")); err != nil {
			return err
		}
	}
	var humans *humansclient.Client
	if *humansURL != "" {
		if humans, err = humansclient.New(*humansURL); err != nil {
			return err
		}
		if listed, err := peopleFrom(ctx, humans, *site); err != nil {
			fmt.Fprintf(os.Stderr, "crew-board: crew-humans: %v; using -people %s until a restart reaches it\n", err, *people)
		} else {
			names = listed
		}
	}
	var infos []board.NetworkInfo
	for _, n := range networks {
		infos = append(infos, n.Info())
	}
	dir := board.FabricDirectory{Fabric: fabric}
	svc, err := board.New(store, dir, time.Now, board.Config{Site: *site, People: names, PeopleRouted: humans != nil, NoticeRetry: *noticeRetry, Networks: infos})
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	syncers := map[string]*network.Syncer{}
	for _, n := range networks {
		n.PullEvery = *pullEvery
		syncer := network.NewSyncer(n, *site, svc, store, time.Now, logger)
		syncers[n.Name] = syncer
		go syncer.Run(ctx)
	}
	svc.OnShare(func() {
		for _, syncer := range syncers {
			syncer.Poke()
		}
	})
	media := board.DefaultMediaConfig(*mediaDir)
	media.Scanner = strings.Fields(*scanner)
	if *noSandbox {
		media.Sandbox = nil
	}
	ingester, err := board.NewIngester(media, time.Now)
	if err != nil {
		return err
	}
	svc.UseMedia(ingester, store)
	notifierConfig := board.NotifierConfig{InstanceID: "crew-board-" + *site, Site: *site, TTL: *noticeTTL, WebURL: *webURL}
	var webOptions []boardweb.Option
	if humans != nil {
		notifierConfig.People = peopleNotifier{humans}
		webOptions = append(webOptions, boardweb.WithInbox(humans))
	}
	notifier := board.NewNotifier(store, dir, fabric, time.Now, notifierConfig)
	svc.OnNotices(notifier.Poke)
	go notifier.Run(ctx, 5*time.Second)
	reindex := map[string]boardhttp.Reindexer{}
	for name, syncer := range syncers {
		reindex[name] = syncer
	}
	server := &http.Server{Addr: *listen, Handler: boardhttp.NewHandler(svc, boardhttp.WithNetworks(reindex, store)), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{server}
	if *webListen != "" {
		web := &http.Server{Addr: *webListen, Handler: boardweb.NewHandler(svc, webOptions...), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, web)
		go func() {
			fmt.Fprintf(os.Stderr, "crew-board: pages on %s\n", *webListen)
			if err := web.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "crew-board: pages:", err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdown)
		}
	}()
	fmt.Fprintf(os.Stderr, "crew-board: site %s serving on %s\n", *site, *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// peopleFrom reads this site's people from crew-humans, trying for a little
// while in case it is starting alongside crew-board.
func peopleFrom(ctx context.Context, humans *humansclient.Client, site string) ([]string, error) {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var listed []humansclient.Person
		var theirs string
		if theirs, listed, err = humans.People(ctx); err == nil {
			if theirs != site {
				return nil, fmt.Errorf("it serves site %s, not %s", theirs, site)
			}
			names := make([]string, 0, len(listed))
			for _, p := range listed {
				names = append(names, p.Name)
			}
			return names, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, err
}

// peopleNotifier hands crew-board's notices to people to crew-humans.
type peopleNotifier struct{ humans *humansclient.Client }

func (p peopleNotifier) NotifyPerson(ctx context.Context, n board.PersonNotice) (string, error) {
	result, err := p.humans.Notify(ctx, humansclient.NotifyRequest{Person: n.Person, Source: "system/crew-board", IdempotencyKey: n.Key, Title: n.Title, Text: n.Text, Link: n.Link, Reason: n.Reason, Group: n.Group, Urgent: n.Urgent})
	var refused *humansclient.HTTPError
	if errors.As(err, &refused) && refused.Status >= 400 && refused.Status < 500 {
		return "", fmt.Errorf("%w: %s", board.ErrPersonRefused, refused.Error())
	}
	if err != nil {
		return "", err
	}
	return result.NoticeID, nil
}
