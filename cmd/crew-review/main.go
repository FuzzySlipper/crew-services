package main

import (
	"context"
	"crew-services/internal/review"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "crew-review:", e)
		os.Exit(1)
	}
}
func run(args []string) error {
	f := flag.NewFlagSet("crew-review", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8413", "loopback listen address")
	db := f.String("db", "crew-review.db", "separate review SQLite database")
	capacity := f.Int("capacity", 2, "maximum running jobs")
	if e := f.Parse(args); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, e := review.OpenSQLite(ctx, *db, review.SystemClock{}, *capacity)
	if e != nil {
		return e
	}
	defer store.Close() // Command admits/readbacks safely; #7414 supplies a real Den client and runtime.
	svc, e := review.New(store, unavailableDen{}, review.UnavailableRuntime{}, "/home/agents/profiles/reviewer/SOUL.md")
	if e != nil {
		return e
	}
	server := &http.Server{Addr: *listen, Handler: review.NewHandler(svc)}
	go func() { <-ctx.Done(); _ = server.Shutdown(context.Background()) }()
	e = server.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

type unavailableDen struct{}

func (unavailableDen) GetReviewContext(context.Context, review.Key) (review.Context, error) {
	return review.Context{}, errors.New("Den client is not configured")
}
func (unavailableDen) FinalizeReview(context.Context, review.Finalization) (review.Receipt, error) {
	return review.Receipt{}, errors.New("Den client is not configured")
}
