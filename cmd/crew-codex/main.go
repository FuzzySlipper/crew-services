// crew-codex projects explicitly selected existing Codex App Server threads
// into crew-services without taking ownership of native turns or tools.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"crew-services/internal/codexadapter"
)

func main() {
	cfg, err := codexadapter.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew-codex: %v\n", err)
		os.Exit(2)
	}
	fabric, err := codexadapter.NewHTTPFabric(cfg.FabricURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew-codex: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := codexadapter.Run(ctx, cfg, fabric, func() (codexadapter.AppServer, error) {
		return codexadapter.StartStdioAppServer(cfg.Command, cfg.CommandArgs)
	}, log.Printf); err != nil {
		fmt.Fprintf(os.Stderr, "crew-codex: %v\n", err)
		os.Exit(1)
	}
}
