// Package windesk runs each windows-desktop session's product instance on the
// Windows playtest box through its agent. The engine backend then drives the
// instance over the LAN; the agent also captures its window and brokers the
// box's one foreground for OS-tier input.
package windesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"crew-services/internal/playtest/session"
)

// Agent is a client of one playtest-windows-agent.
type Agent struct {
	Origin string
	Client *http.Client
}

func NewAgent(origin string) *Agent {
	return &Agent{Origin: strings.TrimRight(origin, "/"), Client: &http.Client{}}
}

// Instance is one product instance the agent started.
type Instance struct {
	ID     string `json:"id"`
	Port   int    `json:"port"`
	Origin string `json:"origin"`
	PID    int    `json:"pid"`
	Log    string `json:"log"`
}

func (a *Agent) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.Origin+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.Client.Do(request)
	if err != nil {
		return fmt.Errorf("windows agent %s: %w", a.Origin, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error  string          `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			if out != nil && len(failure.Result) > 0 && string(failure.Result) != "null" {
				_ = json.Unmarshal(failure.Result, out)
			}
			return errors.New(failure.Error)
		}
		return fmt.Errorf("windows agent %s %s: HTTP %d", method, path, response.StatusCode)
	}
	if out == nil {
		return nil
	}
	if image, ok := out.(*[]byte); ok {
		*image = data
		return nil
	}
	return json.Unmarshal(data, out)
}

func (a *Agent) Start(ctx context.Context, product, holder string) (Instance, error) {
	var instance Instance
	err := a.call(ctx, http.MethodPost, "/v1/instances", map[string]string{"product": product, "holder": holder}, &instance)
	return instance, err
}

func (a *Agent) Stop(ctx context.Context, instance string) error {
	return a.call(ctx, http.MethodDelete, "/v1/instances/"+instance, nil, nil)
}

// Window is the instance window as Windows composes it: world and UI overlay.
func (a *Agent) Window(ctx context.Context, instance string) ([]byte, error) {
	var png []byte
	err := a.call(ctx, http.MethodGet, "/v1/instances/"+instance+"/window.png", nil, &png)
	return png, err
}

// OSInput takes the foreground lease for holder, sends the steps through
// SendInput, and ends the lease, releasing anything still held. A busy
// foreground is refused before anything is sent.
func (a *Agent) OSInput(ctx context.Context, holder, instance string, steps []map[string]any) (map[string]any, error) {
	total := 0
	for _, step := range steps {
		if ms, ok := step["ms"].(float64); ok {
			total += int(ms)
		}
	}
	var lease struct {
		ID      string    `json:"id"`
		Expires time.Time `json:"expires"`
	}
	if err := a.call(ctx, http.MethodPost, "/v1/lease", map[string]any{"holder": holder, "instance": instance, "ttl_ms": total + 10000}, &lease); err != nil {
		return map[string]any{"delivery": "not-sent", "tier": "os"}, err
	}
	receipt := map[string]any{}
	sendErr := a.call(ctx, http.MethodPost, "/v1/lease/"+lease.ID+"/input", map[string]any{"steps": steps}, &receipt)
	endErr := a.call(context.WithoutCancel(ctx), http.MethodDelete, "/v1/lease/"+lease.ID, nil, nil)
	receipt["lease"] = lease.ID
	if endErr != nil {
		receipt["lease_end_error"] = endErr.Error()
	}
	return receipt, sendErr
}

// Launcher starts a windows-desktop session's instance before the inner
// launcher connects to it, and stops it on release.
type Launcher struct {
	Inner session.Launcher
}

func (l *Launcher) Launch(ctx context.Context, id string, p session.Profile) (map[string]any, error) {
	if p.Windows == nil {
		return l.Inner.Launch(ctx, id, p)
	}
	agent := NewAgent(p.Windows.Agent)
	instance, err := agent.Start(ctx, p.Windows.Product, id)
	host := map[string]any{"windows_agent": agent.Origin, "windows_instance": instance.ID, "port": instance.Port,
		"origin": instance.Origin, "pid": instance.PID, "log": instance.Log}
	if err != nil {
		err = fmt.Errorf("start %s on the Windows box: %w", p.Windows.Product, err)
		if instance.ID != "" {
			// The agent could not stop what it started: the session owns it.
			return map[string]any{"host": host}, err
		}
		return nil, err
	}
	windows := *p.Windows
	windows.Instance = instance.ID
	p.Windows, p.URL = &windows, instance.Origin+"/"
	launched, err := l.Inner.Launch(ctx, id, p)
	if launched == nil {
		launched = map[string]any{}
	}
	launched["host"], launched["session_url"] = host, p.URL
	return launched, err
}

// ReleaseHost stops the recorded Windows instance, or defers to the inner
// launcher for other hosts.
func (l *Launcher) ReleaseHost(ctx context.Context, id string, p session.Profile, host map[string]any) (map[string]any, error) {
	instance, _ := host["windows_instance"].(string)
	if instance == "" {
		if releaser, ok := l.Inner.(session.HostReleaser); ok {
			return releaser.ReleaseHost(ctx, id, p, host)
		}
		return map[string]any{"stopped": false, "message": "no host was started"}, nil
	}
	origin, _ := host["windows_agent"].(string)
	if err := NewAgent(origin).Stop(ctx, instance); err != nil {
		return map[string]any{"stopped": false, "instance": instance}, fmt.Errorf("stop Windows instance %s: %w", instance, err)
	}
	return map[string]any{"stopped": true, "instance": instance}, nil
}
