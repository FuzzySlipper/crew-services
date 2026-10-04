package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/playtest/pool"
	"crew-services/internal/playtest/session"
)

func TestReloadFilesAreAtomicAndNonfatal(t *testing.T) {
	dir := t.TempDir()
	gamesPath, poolPath := filepath.Join(dir, "games.json"), filepath.Join(dir, "pool.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := `[{"id":"a","url":"http://localhost/a"}]`
	ab := `[{"id":"a","url":"http://localhost/changed"},{"id":"b","url":"http://localhost/b"}]`
	write(gamesPath, a)
	write(poolPath, `{"size":1}`)
	profiles, config, err := loadConfiguration(gamesPath, poolPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := session.NewRegistry(profiles)
	factory := func(i int) (*session.Service, func(), error) {
		s, err := session.NewWithRegistry(nil, nil, registry, t.TempDir(), "")
		return s, func() {}, err
	}
	first, _, err := factory(0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.NewResizable([]*session.Service{first}, config.wait(), registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	service := &reloadService{pool: p, gamesPath: gamesPath, poolPath: poolPath}
	handler := session.CommandHandler(service)
	reload := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"op":"reload"}`)))
		if w.Code != want {
			t.Fatalf("reload %d: %s", w.Code, w.Body.String())
		}
	}
	write(gamesPath, ab)
	write(poolPath, `{"size":3,"queue_wait_ms":0}`)
	reload(200)
	if p.Activity().(map[string]any)["capacity"] != 3 || len(registry.Profiles()) != 2 {
		t.Fatal("reload not visible")
	}
	for _, bad := range []string{`[`, `null`, `[]`, `[{"id":"a"}]`, `[{"id":"a","url":"a"},{"id":"a","url":"b"}]`, ab + ` {}`, `[{"id":"a","url":"a","typo":true}]`} {
		write(gamesPath, bad)
		write(poolPath, `{"size":4}`)
		reload(400)
		if len(registry.Profiles()) != 2 || p.Activity().(map[string]any)["capacity"] != 3 {
			t.Fatal("bad games file changed configuration")
		}
	}
	for _, bad := range []string{`{`, `null`, `{}`, `{"size":0}`, `{"size":1,"queue_wait_ms":20001}`, `{"size":1,"queue_wait_ms":9223372036854775807}`, `{"size":1,"unknown":true}`} {
		write(gamesPath, a)
		write(poolPath, bad)
		reload(400)
		if len(registry.Profiles()) != 2 || p.Activity().(map[string]any)["capacity"] != 3 {
			t.Fatal("bad pool file changed configuration")
		}
	}
	write(poolPath, `{"size":3}`)
	if err := os.Remove(gamesPath); err != nil {
		t.Fatal(err)
	}
	reload(400)
	write(gamesPath, ab)
	if err := os.Remove(poolPath); err != nil {
		t.Fatal(err)
	}
	reload(400)
	// Service is still usable after every failure.
	v, err := service.Command(context.Background(), session.Request{Op: "games"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v)
	if !strings.Contains(string(data), `"b"`) {
		t.Fatal(string(data))
	}
	write(poolPath, `{"size":2}`)
	reload(200)
}

func TestUnknownGameDistinguishesDiskFromLoadedRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "games.json")
	profiles := []session.Profile{{ID: "a", URL: "http://localhost/a"}}
	registry := session.NewRegistry(profiles)
	slot, err := session.NewWithRegistry(nil, nil, registry, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.NewResizable([]*session.Service{slot}, 0, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	service := &reloadService{pool: p, gamesPath: path}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"a","url":"http://localhost/a"},{"id":"b","url":"http://localhost/b"}]`)
	for _, op := range []string{"game", "start"} {
		_, err := service.Command(context.Background(), session.Request{Op: op, Game: "b"})
		if err == nil || !strings.Contains(err.Error(), "in the profile file but not loaded; run 'playtest reload'") {
			t.Fatal(err)
		}
	}
	if len(registry.Profiles()) != 1 {
		t.Fatal("diagnostic reloaded configuration")
	}
	_, err = service.Command(context.Background(), session.Request{Op: "game", Game: "missing"})
	if err == nil || !strings.Contains(err.Error(), "absent from both") {
		t.Fatal(err)
	}
	write(`[`)
	_, err = service.Command(context.Background(), session.Request{Op: "game", Game: "b"})
	if err == nil || !strings.Contains(err.Error(), "could not inspect") {
		t.Fatal(err)
	}
	write(`[{"id":"a","url":"http://localhost/a"},{"id":"b","url":"http://localhost/b"}]`)
	if _, err = service.Command(context.Background(), session.Request{Op: "reload"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Command(context.Background(), session.Request{Op: "game", Game: "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolLifecycleDefaultsAndBounds(t *testing.T) {
	dir := t.TempDir()
	gamesPath, poolPath := filepath.Join(dir, "games.json"), filepath.Join(dir, "pool.json")
	if err := os.WriteFile(gamesPath, []byte(`[{"id":"a","url":"http://localhost/a"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]pool.Lifecycle{
		`{"size":1}`: pool.DefaultLifecycle,
		`{"size":1,"idle_timeout_minutes":0,"history_retention_days":0}`:  {},
		`{"size":1,"idle_timeout_minutes":90,"history_retention_days":3}`: {IdleTimeout: 90 * time.Minute, Retention: 3 * 24 * time.Hour},
	} {
		if err := os.WriteFile(poolPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, config, err := loadConfiguration(gamesPath, poolPath)
		if err != nil || config.lifecycle() != want {
			t.Fatalf("%s: lifecycle %+v %v, want %+v", body, config.lifecycle(), err, want)
		}
	}
	for _, body := range []string{`{"size":1,"idle_timeout_minutes":-1}`, `{"size":1,"history_retention_days":-2}`} {
		if err := os.WriteFile(poolPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConfiguration(gamesPath, poolPath); err == nil {
			t.Fatalf("%s accepted", body)
		}
	}
}
