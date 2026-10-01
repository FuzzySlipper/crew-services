//go:build !windows

package main

import (
	"log"

	"crew-services/internal/winagent"
)

func newDesktop() winagent.Desktop {
	log.Fatal("playtest-windows-agent runs on Windows")
	return nil
}
