// playtest-target is the loopback target process used by the DSH Wolf adapter.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"crew-services/internal/playtest/target"
)

func main() {
	socket := flag.String("socket", "", "Wolf Unix socket path")
	state := flag.String("state", "", "durable target state directory")
	clientID := flag.String("client-id", "", "paired Moonlight client identity")
	videoCaps := flag.String("video-producer-buffer-caps", "", "Wolf video producer caps; den-srv VAAPI: video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}")
	runnerStateFolder := flag.String("runner-state-folder", "", "Wolf runner state folder below playtest/user (default follows the profile runner name)")
	runnerContainerName := flag.String("runner-container-name", "", "Wolf Docker runner name (default follows the profile runner name)")
	wolfStateDir := flag.String("wolf-state-dir", "", "Wolf host state directory; enables per-session Firefox input isolation")
	port := flag.Int("port", target.DefaultPort(), "loopback command port")
	flag.Parse()
	if *socket == "" || *state == "" || *clientID == "" {
		fmt.Fprintln(os.Stderr, "--socket, --state, and --client-id are required")
		os.Exit(2)
	}
	controller, err := target.NewController(target.NewUnixWolf(*socket), *state, *clientID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	controller.SetVideoProducerBufferCaps(*videoCaps)
	if err := controller.SetWolfStateDir(*wolfStateDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := controller.SetBrowserRunner(*runnerStateFolder, *runnerContainerName); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: target.Handler(controller)}
	stop := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
				if err := controller.Sweep(context.Background()); err != nil {
					fmt.Fprintln(os.Stderr, "Lease cleanup pending:", err)
				}
			}
		}
	}()
	go func() {
		<-stop
		close(done)
		_ = controller.Close(context.Background())
		_ = server.Shutdown(context.Background())
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
