package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"crew-services/internal/playtest/pool"
	"crew-services/internal/playtest/session"
)

type poolConfig struct {
	Size        int  `json:"size"`
	QueueWaitMS *int `json:"queue_wait_ms,omitempty"`
}

func (c poolConfig) wait() time.Duration {
	if c.QueueWaitMS != nil {
		return time.Duration(*c.QueueWaitMS) * time.Millisecond
	}
	return 15 * time.Second
}

// Only the two reloadable files are read. All adapter construction settings
// were captured at startup, and Pool owns the atomic admission/state decision.
type reloadService struct {
	mu                  sync.Mutex
	pool                *pool.Pool
	gamesPath, poolPath string
	single              *session.Service
}

func (s *reloadService) Command(ctx context.Context, request session.Request) (any, error) {
	if request.Op != "reload" {
		var value any
		var err error
		if s.single != nil {
			value, err = s.single.Command(ctx, request)
		} else {
			value, err = s.pool.Command(ctx, request)
		}
		var missing *session.UnknownGameError
		if errors.As(err, &missing) {
			return value, s.unknownGame(missing)
		}
		return value, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, config, err := loadConfiguration(s.gamesPath, s.poolPath)
	if err != nil {
		return nil, err
	}
	if err := s.pool.Reload(profiles, config.Size, config.wait()); err != nil {
		return nil, err
	}
	return map[string]any{"reloaded": true, "profiles": len(profiles), "capacity": config.Size, "queue_wait_ms": config.wait().Milliseconds()}, nil
}
func loadConfiguration(gamesPath, poolPath string) ([]session.Profile, poolConfig, error) {
	var profiles []session.Profile
	config := poolConfig{Size: 1}
	if err := readJSON(gamesPath, &profiles); err != nil {
		return nil, config, err
	}
	if err := session.ValidateProfiles(profiles); err != nil {
		return nil, config, fmt.Errorf("games: %w", err)
	}
	if poolPath != "" {
		config = poolConfig{}
		if err := readJSON(poolPath, &config); err != nil {
			return nil, config, err
		}
	}
	if config.Size < 1 {
		return nil, config, fmt.Errorf("pool size must be positive")
	}
	// Check integers before converting milliseconds to Duration, avoiding overflow.
	if config.QueueWaitMS != nil && (*config.QueueWaitMS < 0 || *config.QueueWaitMS > 20000) {
		return nil, config, fmt.Errorf("queue_wait_ms must be 0..20000")
	}
	return profiles, config, nil
}
func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("read %s: expected one JSON value", path)
	}
	return nil
}

func (s *reloadService) unknownGame(missing *session.UnknownGameError) error {
	var profiles []session.Profile
	err := readJSON(s.gamesPath, &profiles)
	if err == nil {
		err = session.ValidateProfiles(profiles)
	}
	if err != nil {
		return fmt.Errorf("%w; could not inspect the profile file: %v; fix it and run 'playtest reload'", missing, err)
	}
	for _, p := range profiles {
		if p.ID == missing.ID {
			return fmt.Errorf("unknown game %q: it is in the profile file but not loaded; run 'playtest reload'", missing.ID)
		}
	}
	return fmt.Errorf("unknown game %q: absent from both the loaded registry and the profile file; use 'playtest games', or add the profile and run 'playtest reload'", missing.ID)
}
