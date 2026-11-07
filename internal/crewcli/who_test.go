package crewcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestCurrentKeepsRunningRecentOwnAndSeated(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-recentFor - time.Hour)
	sessions := []SessionInfo{
		{Address: "s/busy", NativeID: "busy", State: "busy", SeenAt: old},
		{Address: "s/idle", NativeID: "idle", State: "idle", SeenAt: old},
		{Address: "s/error", NativeID: "error", State: "error", SeenAt: old},
		{Address: "s/recent-exit", NativeID: "recent-exit", State: "exited", SeenAt: now.Add(-recentFor + time.Hour)},
		{Address: "s/recent-unloaded", NativeID: "recent-unloaded", State: "not loaded", SeenAt: now.Add(-time.Hour)},
		{Address: "s/self", NativeID: "self", State: "exited", SeenAt: old},
		{Address: "s/seated", NativeID: "seated", State: "exited", SeenAt: old},
		{Address: "s/old-exit", NativeID: "old-exit", State: "exited", SeenAt: old},
		{Address: "s/old-unloaded", NativeID: "old-unloaded", State: "not loaded", SeenAt: old},
		{Address: "codex/pinned", NativeID: "pinned", State: "not loaded"},
	}
	seats := map[string][]string{"s/seated": {"project.lead"}}

	current, hidden := Current(sessions, "self", seats, now)

	if hidden != 3 {
		t.Fatalf("hidden = %d, want 3: %+v", hidden, current)
	}
	got := []string{}
	for _, s := range current {
		got = append(got, s.NativeID)
	}
	want := "busy idle error recent-exit recent-unloaded self seated"
	if strings.Join(got, " ") != want {
		t.Fatalf("kept %v, want %s", got, want)
	}
}

func TestResolveStillFindsASessionWhoLeavesOut(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	sessions := []SessionInfo{{Address: "s/abc123", NativeID: "old-session", State: "exited", SeenAt: now.Add(-30 * 24 * time.Hour)}}
	if current, _ := Current(sessions, "", nil, now); len(current) != 0 {
		t.Fatalf("old session still listed: %+v", current)
	}
	address, err := Resolve("old-sess", sessions)
	if err != nil || address != "s/abc123" {
		t.Fatalf("Resolve = %q, %v", address, err)
	}
}

func TestWhoRefusesAnUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"who", "--everything"}, "", "", strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--all") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
