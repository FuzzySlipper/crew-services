// Command playtest-windows-agent runs product instances on the Windows
// playtest box and brokers its foreground for OS-tier input. Start it at
// logon in the interactive console session (see docs/playtest-windows.md).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"crew-services/internal/winagent"
)

func main() {
	executable, _ := os.Executable()
	config := flag.String("config", filepath.Join(filepath.Dir(executable), "agent.json"), "agent configuration")
	flag.Parse()
	settings, err := winagent.LoadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(settings.Logs, 0o755); err != nil {
		log.Fatal(err)
	}
	agent := winagent.New(settings, newDesktop())
	if stopped := agent.ReapLeftovers(); len(stopped) > 0 {
		log.Printf("stopped instances a previous run left: %v", stopped)
	}
	server := &http.Server{Addr: settings.Listen, Handler: agent.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		agent.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("playtest windows agent on %s", settings.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
