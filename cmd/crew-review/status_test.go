package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crew-services/internal/review"
)

func TestStatusShowsActiveWaitingAndRecent(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	pool := review.Snapshot{
		Backend: "codex", Capacity: 4, Running: 1,
		Active:  []review.Projection{{Key: review.Key{ProjectID: "crew-services", TaskID: 10564, ReviewRoundID: 7210}, State: review.Running, CreatedAt: now.Add(-3 * time.Minute)}},
		Waiting: []review.WaitingSubmission{{ProjectID: "rusty-engine", TaskID: 10640, Phase: review.SubmissionGatePending, CreatedAt: now.Add(-90 * time.Second)}},
		Recent:  []review.Projection{{Key: review.Key{ProjectID: "rusty-engine", TaskID: 10634, ReviewRoundID: 7325}, State: review.Succeeded, Receipt: &review.Receipt{Verdict: "looks_good"}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}},
	}
	var out bytes.Buffer
	printStatus(&out, pool, 5, now)
	for _, want := range []string{"codex backend, capacity 4: 1 running, 0 queued, 0 finalizing, 1 waiting", "10564", "7210", "gate_pending", "10640", "looks_good", "1h ago"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStatusReadsTheRunningService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/review-pool" || r.URL.Query().Get("limit") != "3" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(review.Snapshot{Backend: "codex", Capacity: 2})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	getenv := func(name string) string {
		if name == "CREW_REVIEW_URL" {
			return server.URL
		}
		return ""
	}
	if code := status(context.Background(), []string{"--limit", "3"}, getenv, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "capacity 2") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	server.Close()
	if code := status(context.Background(), nil, getenv, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not answering") {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
}
