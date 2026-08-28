package reviewden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"crew-services/internal/review"
)

const reviewDenTestSHA = "0123456789abcdef0123456789abcdef01234567"

func TestGetReviewContextMapsTypedStructuredContent(t *testing.T) {
	var gotRequest struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	structured := map[string]any{
		"schema": "den_review.reviewer_context.v1", "schema_version": 1,
		"project_id": "dsh-crew", "task_id": 7416,
		"task":            map[string]any{"id": 7416, "project_id": "dsh-crew", "root_path": "/home/dev/dsh-crew"},
		"current_round":   map[string]any{"id": 12, "project_id": "dsh-crew", "task_id": 7416},
		"next_state":      "source_review_ready",
		"prior_findings":  []map[string]any{{"id": "R7416-1", "summary": "recheck claim expiry"}},
		"rereview_packet": map[string]any{"round": 12, "addresses": []string{"R7416-1"}},
	}
	wantMaterial, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeToolResult(w, structured)
	}))
	defer server.Close()
	client, err := New(server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	key := review.Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 12, CorrelationID: "corr"}
	got, err := client.GetReviewContext(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != key || got.NextState != "source_review_ready" || got.Workspace != "/home/dev/dsh-crew" {
		t.Fatalf("context = %+v", got)
	}
	if !bytes.Equal(got.Material, wantMaterial) {
		t.Fatalf("material = %s, want %s", got.Material, wantMaterial)
	}
	if gotRequest.Method != "tools/call" || gotRequest.Params.Name != "get_review_context" {
		t.Fatalf("request = %+v", gotRequest)
	}
	var args map[string]any
	if err := json.Unmarshal(gotRequest.Params.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, map[string]any{"task_id": float64(7416)}) {
		t.Fatalf("arguments = %#v", args)
	}
}

func TestGetReviewContextTypedNoCurrentRoundIsStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeToolResult(w, map[string]any{
			"schema": "den_review.reviewer_context.v1", "schema_version": 1,
			"task_id": 7416, "error_code": "review_context_unavailable", "reason": "no_current_round", "retryable": false,
		})
	}))
	defer server.Close()
	client, err := New(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetReviewContext(context.Background(), review.Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 12, CorrelationID: "corr"})
	if !errors.Is(err, review.ErrStaleRound) {
		t.Fatalf("error = %v, want stale round", err)
	}
}

func TestRequestReviewAndGitHubGateMethodsMapDenAuthority(t *testing.T) {
	var calls []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		var arguments map[string]any
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			t.Fatalf("decode arguments: %v", err)
		}
		calls = append(calls, map[string]any{"name": request.Params.Name, "arguments": arguments})
		switch request.Params.Name {
		case "request_review":
			writeToolResult(w, map[string]any{"id": 12, "project_id": "dsh-crew", "task_id": 7416, "review_round_id": 12})
		case "watch_github_checks":
			writeToolResult(w, map[string]any{
				"id": 44, "project_id": "dsh-crew", "task_id": 7416, "repository": "owner/repo",
				"commit_sha": reviewDenTestSHA, "ref": "main", "status": "pending",
			})
		case "get_github_check_gate":
			writeToolResult(w, map[string]any{
				"id": 44, "project_id": "dsh-crew", "task_id": 7416, "repository": "owner/repo",
				"commit_sha": reviewDenTestSHA, "ref": "main", "status": "failed",
				"terminal_reason": "required_checks_missing", "failure_summary": "lint was not observed",
			})
		default:
			t.Fatalf("unexpected Den tool %q", request.Params.Name)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := review.SubmissionRequest{
		ProjectID: "dsh-crew", TaskID: 7416, Repository: "owner/repo", CommitSHA: reviewDenTestSHA,
		Ref: "main", RequiredChecks: []string{"lint"}, ReviewSummary: "Please review this exact commit.", Reviewer: "@reviewer",
	}
	round, err := client.RequestReview(context.Background(), request)
	if err != nil || round.ID != 12 || round.ProjectID != request.ProjectID || round.TaskID != request.TaskID {
		t.Fatalf("round=%+v err=%v", round, err)
	}
	gateRequest := review.GateRequest{ProjectID: request.ProjectID, TaskID: request.TaskID, Repository: request.Repository, CommitSHA: request.CommitSHA, Ref: request.Ref, RequiredChecks: request.RequiredChecks, RequestedBy: request.Reviewer}
	pending, err := client.WatchGitHubChecks(context.Background(), gateRequest)
	if err != nil || pending.Handle != "44" || pending.Status != "pending" || pending.CommitSHA != request.CommitSHA {
		t.Fatalf("pending gate=%+v err=%v", pending, err)
	}
	failed, err := client.GetGitHubCheckGate(context.Background(), gateRequest)
	if err != nil || failed.Status != "failed" || failed.TerminalReason != "required_checks_missing" || failed.FailureSummary != "lint was not observed" {
		t.Fatalf("failed gate=%+v err=%v", failed, err)
	}
	if len(calls) != 3 || calls[0]["name"] != "request_review" || calls[1]["name"] != "watch_github_checks" || calls[2]["name"] != "get_github_check_gate" {
		t.Fatalf("calls=%v", calls)
	}
	requestArgs := calls[0]["arguments"].(map[string]any)
	if requestArgs["task_id"] != float64(7416) || requestArgs["requested_by"] != "@reviewer" || !strings.Contains(requestArgs["notes"].(string), reviewDenTestSHA) {
		t.Fatalf("request args=%#v", requestArgs)
	}
	if _, present := requestArgs["project_id"]; present {
		t.Fatal("task-scoped request_review must not send project_id")
	}
}

func TestFinalizeReviewMapsReceiptAndArguments(t *testing.T) {
	var gotArgs map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Params.Name != "finalize_review" {
			t.Fatalf("tool = %q", request.Params.Name)
		}
		if err := json.Unmarshal(request.Params.Arguments, &gotArgs); err != nil {
			t.Fatalf("decode arguments: %v", err)
		}
		writeToolResult(w, map[string]any{
			"schema": "den_review.completion_receipt.v1", "schema_version": 1,
			"id": 99, "project_id": "dsh-crew", "task_id": 7416, "review_round_id": 12,
			"verdict": "changes_requested", "target_task_status": "in_progress", "state": "complete",
			"packet_id": 1001, "packet_message_id": 1002, "message_id": 1003,
			"resulting_task_status": "in_progress",
		})
	}))
	defer server.Close()
	client, err := New(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	key := review.Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 12, CorrelationID: "corr"}
	receipt, err := client.FinalizeReview(context.Background(), review.Finalization{
		Key: key, Reviewer: "reviewer-codex",
		Completion: review.Completion{
			Verdict: "changes_requested", Notes: "please fix this", Evidence: "go test ./... passed",
			PriorResolutions: []review.FindingResolution{{FindingID: 7, Status: "verified_fixed", VerificationNote: "tested"}},
			NewFindings:      []review.NewFinding{{Category: "blocking_bug", Summary: "broken", Notes: "details", FileReferences: []string{"x.go:1"}, TestCommands: []string{"go test ./..."}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FinalizationID != 99 || receipt.PacketID != 1001 || receipt.ResultingTaskStatus != "in_progress" || receipt.Verdict != "changes_requested" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if gotArgs["review_round_id"] != float64(12) || gotArgs["decided_by"] != "reviewer-codex" || gotArgs["notes"] != "please fix this\n\nEvidence:\ngo test ./... passed" {
		t.Fatalf("arguments = %#v", gotArgs)
	}
	if _, ok := gotArgs["project_id"]; ok {
		t.Fatal("project_id must not be sent to finalize_review")
	}
}

func TestFinalizeReviewClassifiesStaleAndConflict(t *testing.T) {
	for name, outcome := range map[string]struct {
		message string
		want    error
	}{
		"stale": {
			message: `{"error":"den_backend_request_failed","message":"{\"error\":{\"code\":\"stale_review_round\",\"message\":\"round is no longer current\"}}"}`,
			want:    review.ErrStaleRound,
		},
		"conflict": {
			message: `{"error":"den_backend_request_failed","message":"{\"error\":{\"code\":\"review_finalization_conflict\",\"message\":\"different decision identity\"}}"}`,
			want:    review.ErrDenConflict,
		},
		"rejected": {
			message: `{"error":"den_backend_request_failed","message":"{\"error\":{\"code\":\"review_request_too_large\",\"message\":\"review request exceeds the bounded payload budget\"}}"}`,
			want:    review.ErrDenRejected,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeErrorToolResult(w, outcome.message)
			}))
			client, err := New(server.URL, "", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.FinalizeReview(context.Background(), review.Finalization{
				Key: review.Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 12, CorrelationID: "corr"}, Reviewer: "reviewer",
				Completion: review.Completion{Verdict: "looks_good"},
			})
			server.Close()
			if !errors.Is(err, outcome.want) {
				t.Fatalf("error = %v, want %v", err, outcome.want)
			}
		})
	}
}

func TestValidateFinalizationUsesEncodedDenRequestSize(t *testing.T) {
	client, err := New("http://den.test/mcp", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	finalization := review.Finalization{
		Key:        review.Key{ProjectID: "dsh-crew", TaskID: 7416, ReviewRoundID: 12, CorrelationID: "corr"},
		Reviewer:   "reviewer",
		Completion: review.Completion{Verdict: "looks_good", Notes: strings.Repeat(">", 700)},
	}
	if err := client.ValidateFinalization(finalization); !errors.Is(err, review.ErrDenRejected) {
		t.Fatalf("encoded oversize validation error = %v", err)
	}
	finalization.Completion.Notes = "concise"
	if err := client.ValidateFinalization(finalization); err != nil {
		t.Fatalf("concise finalization rejected: %v", err)
	}
}

func TestNewRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "localhost:5199/mcp", "ftp://den/mcp", "http://den/mcp#fragment"} {
		if _, err := New(endpoint, "", nil); err == nil {
			t.Fatalf("New(%q) succeeded", endpoint)
		}
	}
}

func writeToolResult(w http.ResponseWriter, structured any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": "1",
		"result": map[string]any{
			"content": []map[string]string{{"type": "text", "text": "ok"}},
			"isError": false, "structuredContent": structured,
		},
	})
}

func writeErrorToolResult(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": "1",
		"result": map[string]any{
			"content": []map[string]string{{"type": "text", "text": message}},
			"isError": true, "structuredContent": json.RawMessage(message),
		},
	})
}
