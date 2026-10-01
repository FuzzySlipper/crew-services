package main

import (
	"fmt"
	"path/filepath"

	"crew-services/internal/playtest/browser"
	"crew-services/internal/playtest/engine"
	"crew-services/internal/playtest/hosting"
	"crew-services/internal/playtest/routing"
	"crew-services/internal/playtest/session"
	"crew-services/internal/playtest/windesk"
)

// Immutable startup settings. Reload never opens machine configuration again.
type slotBuilder struct {
	state, worker, browserWorker, chromium string
	registry                               *session.Registry
	// hosts starts session-owned product hosts; shared with den-serve's state.
	hosts    hosting.Hosts
	manifest hosting.ManifestReader
	locks    *hosting.RepoLocks
}

func (b slotBuilder) create(index int) (*session.Service, func(), error) {
	state := b.state
	if index > 0 {
		state = filepath.Join(state, "slots", fmt.Sprintf("slot-%d", index+1))
	}
	router := &routing.Router{Entries: map[string]routing.Entry{}}
	var releases []func()
	cleanup := func() {
		for _, release := range releases {
			release()
		}
	}
	if b.browserWorker != "" {
		backend, err := browser.New(browser.Config{State: filepath.Join(state, "browser"), Worker: b.browserWorker, Chromium: b.chromium})
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		releases = append(releases, func() { backend.Close() })
		router.Entries["browser"] = routing.Entry{Backend: backend, Launcher: backend}
	}
	// The engine backend drives a product host directly; it owns no process.
	direct, err := engine.New(engine.Config{State: filepath.Join(state, "engine"), Label: fmt.Sprintf("crew-playtest-%d", index+1)})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	releases = append(releases, func() { direct.Close() })
	router.Entries["engine"] = routing.Entry{Backend: direct, Launcher: direct}
	launcher := &windesk.Launcher{Inner: &hosting.Launcher{Inner: router, Hosts: b.hosts, Manifest: b.manifest, Locks: b.locks}}
	service, err := session.NewWithRegistry(router, launcher, b.registry, state, b.worker)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return service, cleanup, nil
}
