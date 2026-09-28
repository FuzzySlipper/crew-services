package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"crew-services/internal/playtest/browser"
	"crew-services/internal/playtest/routing"
	"crew-services/internal/playtest/session"
	"crew-services/internal/playtest/wolf"
)

// Immutable startup settings. Reload never opens machine configuration again.
type slotBuilder struct {
	state, worker, browserWorker, chromium, forward string
	wolfEnabled                                     bool
	wolfConfigs                                     map[int]wolf.Config
	registry                                        *session.Registry
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
	if b.wolfEnabled {
		config, ok := b.wolfConfigs[index]
		if !ok {
			return nil, nil, fmt.Errorf("Wolf slot-%d was not provisioned at startup", index+1)
		}
		backend, err := wolf.New(config)
		if err != nil {
			return nil, nil, err
		}
		releases = append(releases, func() { backend.Close() })
		launcher := &session.WolfLauncher{Backend: backend, SSHHost: config.SSHHost, ForwardBinary: b.forward}
		router.Entries["wolf"] = routing.Entry{Backend: backend, Launcher: launcher}
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
	service, err := session.NewWithRegistry(router, router, b.registry, state, b.worker)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return service, cleanup, nil
}

func loadWolfConfigs(configPath, poolPath string) (map[int]wolf.Config, error) {
	configs := map[int]wolf.Config{}
	if configPath == "" {
		return configs, nil
	}
	if poolPath == "" {
		var config wolf.Config
		if err := readJSON(configPath, &config); err != nil {
			return nil, err
		}
		configs[0] = config
		return configs, nil
	}
	// Provisioned native targets are captured once. Browser-only slots need no
	// per-slot machine files. Native capacity cannot exceed provisioned devices.
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(poolPath), "slots", "slot-*", "machine.json"))
	if err != nil {
		return nil, err
	}
	targets, captures, moonlight := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		number, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(filepath.Dir(path)), "slot-"))
		if err != nil || number < 1 {
			return nil, fmt.Errorf("invalid slot path %s", path)
		}
		var config wolf.Config
		if err := readJSON(path, &config); err != nil {
			return nil, err
		}
		target := fmt.Sprintf("%s:%d", config.SSHHost, config.TargetPort)
		capture, _ := filepath.Abs(config.State)
		client, _ := filepath.Abs(config.MoonlightConfig)
		if targets[target] || captures[capture] || moonlight[client] {
			return nil, fmt.Errorf("pool slots must have distinct native targets, capture directories and Moonlight configurations")
		}
		targets[target], captures[capture], moonlight[client] = true, true, true
		configs[number-1] = config
	}
	return configs, nil
}
