package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"crew-services/internal/app"
	"crew-services/internal/config"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "crew-messaging:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	persistence, err := sqlite.Open(ctx, cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer persistence.Close()
	svc, err := service.New(persistence, service.SystemClock{}, service.WithMaxLeaseDuration(cfg.LeaseDuration), service.WithMaxTTLDuration(cfg.TTLDuration), service.WithRetention(cfg.Retention))
	if err != nil {
		return err
	}
	application, err := app.New(cfg, svc)
	if err != nil {
		return err
	}
	go retain(ctx, svc, retentionInterval)
	if err := application.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

const retentionInterval = time.Hour

// retain prunes expired operation receipts at startup and then on a fixed
// interval. It only deletes receipts; it never claims, wakes, or settles work.
func retain(ctx context.Context, svc *service.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		removed, err := svc.PruneDeliveryOperations(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "crew-messaging: prune operation receipts:", err)
		} else if removed > 0 {
			fmt.Fprintf(os.Stderr, "crew-messaging: pruned %d operation receipts\n", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
