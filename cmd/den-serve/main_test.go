package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"crew-services/internal/devserver"
)

type fakeRestartManager struct {
	session   devserver.SessionState
	statusErr error
	calls     []string
}

func (m *fakeRestartManager) Status(context.Context, devserver.StatusOptions) (devserver.SessionState, error) {
	m.calls = append(m.calls, "status")
	return m.session, m.statusErr
}

func (m *fakeRestartManager) Stop(context.Context, devserver.StopOptions) (devserver.StopResult, error) {
	m.calls = append(m.calls, "stop")
	return devserver.StopResult{}, nil
}

func (m *fakeRestartManager) Up(context.Context, devserver.UpOptions) (devserver.UpResult, error) {
	m.calls = append(m.calls, "up")
	return devserver.UpResult{Session: devserver.SessionState{Project: "alpha"}}, nil
}

func TestNewManagerUsesBuiltInDefaultsWhenConfigPathIsEmpty(t *testing.T) {
	if _, err := newManager(""); err != nil {
		t.Fatalf("newManager(\"\") error = %v", err)
	}
}

func TestFormatSessionPacketExposesLaunchFreshness(t *testing.T) {
	session := devserver.SessionState{
		Project:     "alpha",
		Status:      "stale",
		LocalURL:    "http://127.0.0.1:5173/",
		StatePath:   "/tmp/current.json",
		SessionDir:  "/tmp/session",
		ReuseSource: "restarted_stale",
		StartedAt:   time.Date(2026, time.July, 25, 17, 0, 0, 0, time.UTC),
		PID:         123,
		LaunchFingerprint: devserver.LaunchFingerprint{
			Value:    "launch-value",
			RepoHead: "launch-head",
		},
		CurrentFingerprint: devserver.LaunchFingerprint{Value: "current-value"},
		Stale:              true,
		StaleReason:        "repo HEAD changed",
	}
	packet := formatSessionPacket(session)
	for _, marker := range []string{
		"alpha stale",
		"launch source: restarted_stale",
		"started: 2026-07-25T17:00:00Z",
		"launch fingerprint:  launch-value",
		"current fingerprint: current-value",
		"launch repo HEAD:    launch-head",
		"stale: true (repo HEAD changed)",
	} {
		if !strings.Contains(packet, marker) {
			t.Fatalf("packet missing %q:\n%s", marker, packet)
		}
	}
}

func TestRestartStopsRunningBrokerOwnedSessionBeforeUp(t *testing.T) {
	manager := &fakeRestartManager{session: devserver.SessionState{Status: "running", Ownership: "broker_owned"}}
	if _, err := restart(context.Background(), manager, devserver.UpOptions{Project: "alpha"}); err != nil {
		t.Fatalf("restart() error = %v", err)
	}
	if got := strings.Join(manager.calls, ","); got != "status,stop,up" {
		t.Fatalf("calls = %q, want status,stop,up", got)
	}
}

func TestRestartActsLikeUpWhenNoSessionExists(t *testing.T) {
	manager := &fakeRestartManager{statusErr: devserver.ErrSessionNotFound}
	if _, err := restart(context.Background(), manager, devserver.UpOptions{Project: "alpha"}); err != nil {
		t.Fatalf("restart() error = %v", err)
	}
	if got := strings.Join(manager.calls, ","); got != "status,up" {
		t.Fatalf("calls = %q, want status,up", got)
	}
}

func TestRestartRefusesUnownedSession(t *testing.T) {
	manager := &fakeRestartManager{session: devserver.SessionState{Status: "unowned", Ownership: "unowned"}}
	_, err := restart(context.Background(), manager, devserver.UpOptions{Project: "alpha"})
	if err == nil || !strings.Contains(err.Error(), "not broker-owned") {
		t.Fatalf("restart() error = %v, want broker ownership error", err)
	}
	if got := strings.Join(manager.calls, ","); got != "status" {
		t.Fatalf("calls = %q, want status", got)
	}
}

func TestRestartReturnsUnexpectedStatusError(t *testing.T) {
	manager := &fakeRestartManager{statusErr: errors.New("status failed")}
	_, err := restart(context.Background(), manager, devserver.UpOptions{Project: "alpha"})
	if err == nil || err.Error() != "status failed" {
		t.Fatalf("restart() error = %v, want status failed", err)
	}
}
