package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	svc, err := service.New(persistence, service.SystemClock{}, service.WithMaxLeaseDuration(cfg.LeaseDuration), service.WithMaxTTLDuration(cfg.TTLDuration))
	if err != nil {
		return err
	}
	application, err := app.New(cfg, svc)
	if err != nil {
		return err
	}
	if err := application.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
