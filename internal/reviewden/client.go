// Package reviewden adapts the Den MCP review tools to the review runner.
//
// Den remains the authority for review context and finalization. This package
// only translates the small typed MCP boundary; it does not cache Den state or
// decide whether a review should run.
package reviewden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"crew-services/internal/review"
)

const (
	DefaultMCPURL    = "http://192.168.1.10:5199/mcp"
	maxResponseBytes = 1 << 20
)

// Client is a deliberately small stateless caller for the Den MCP endpoint.
// Den's current local endpoint accepts direct tools/call requests, so no
// MCP-session state is kept here. A caller can provide an HTTP client with a
// bounded timeout for tests or process-specific policy.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	request  atomic.Uint64
}

var _ review.SubmissionDenClient = (*Client)(nil)

// New constructs a Den MCP client after validating its endpoint.
func New(endpoint, token string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("Den MCP URL must be an absolute http or https URL")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("Den MCP URL must not contain a fragment")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 65 * time.Second}
	}
	return &Client{endpoint: parsed.String(), token: strings.TrimSpace(token), http: httpClient}, nil
}

// FromEnv uses the same names as the existing Den tooling. An unset URL uses
// the trusted agent-box default rather than silently constructing a partial
// review service.
func FromEnv() (*Client, error) {
	endpoint := strings.TrimSpace(os.Getenv("DEN_MCP_URL"))
	if endpoint == "" {
		endpoint = DefaultMCPURL
	}
	return New(endpoint, os.Getenv("DEN_MCP_TOKEN"), nil)
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolEnvelope struct {
	Content           []toolContent   `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *Client) call(ctx context.Context, operation string, arguments any) (json.RawMessage, error) {
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encode Den %s arguments: %w", operation, err)
	}
	requestID := strconv.FormatUint(c.request.Add(1), 10)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      operation,
			"arguments": json.RawMessage(encodedArguments),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode Den MCP request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Den MCP request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Den MCP %s: %w", operation, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Den MCP %s response: %w", operation, err)
	}
	if len(responseBody) > maxResponseBytes {
		return nil, fmt.Errorf("Den MCP %s response exceeded %d bytes", operation, maxResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Den MCP %s returned HTTP %d: %s", operation, response.StatusCode, boundedText(responseBody, 4096))
	}
	var rpc rpcResponse
	if err := json.Unmarshal(responseBody, &rpc); err != nil {
		return nil, fmt.Errorf("decode Den MCP %s response: %w", operation, err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("Den MCP %s JSON-RPC error %d: %s", operation, rpc.Error.Code, rpc.Error.Message)
	}
	if len(rpc.Result) == 0 || !json.Valid(rpc.Result) {
		return nil, fmt.Errorf("Den MCP %s response is missing a valid result", operation)
	}
	var envelope toolEnvelope
	if err := json.Unmarshal(rpc.Result, &envelope); err != nil {
		return nil, fmt.Errorf("decode Den MCP %s tool result: %w", operation, err)
	}
	if envelope.IsError {
		return nil, classifyToolError(operation, envelope)
	}
	if len(envelope.StructuredContent) == 0 || !json.Valid(envelope.StructuredContent) {
		return nil, fmt.Errorf("Den MCP %s returned no structured JSON", operation)
	}
	return envelope.StructuredContent, nil
}

func classifyToolError(operation string, envelope toolEnvelope) error {
	message := "operation returned an error"
	if len(envelope.Content) > 0 && strings.TrimSpace(envelope.Content[0].Text) != "" {
		message = boundedText([]byte(envelope.Content[0].Text), 4096)
	}
	// The MCP facade normally returns a den_backend_request_failed object whose
	// message contains the review service's {error:{code,message}} body. Accept
	// both that wrapper and direct error objects so the adapter remains useful
	// across the current local Den deployments.
	var outer struct {
		Error   string          `json:"error"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal([]byte(message), &outer) == nil {
		inner := string(outer.Message)
		var quoted string
		if json.Unmarshal(outer.Message, &quoted) == nil {
			inner = quoted
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(outer.Message, &body) == nil {
			if body.Error.Code != "" {
				return classifyCode(operation, body.Error.Code, body.Error.Message)
			}
			if body.Code != "" {
				return classifyCode(operation, body.Code, body.Message)
			}
		}
		if inner != "" && inner != "null" {
			message = inner
		}
		if outer.Error != "" {
			message = outer.Error + ": " + message
		}
	}
	return classifyCode(operation, "", message)
}

func classifyCode(operation, code, message string) error {
	text := strings.TrimSpace(strings.TrimSpace(code) + ": " + strings.TrimSpace(message))
	lower := strings.ToLower(text)
	if strings.Contains(lower, "stale_review_round") || strings.Contains(lower, "review round is no longer current") || strings.Contains(lower, "round is stale") {
		return fmt.Errorf("%w: %s", review.ErrStaleRound, text)
	}
	if strings.Contains(lower, "review_finalization_conflict") || strings.Contains(lower, "finalization conflict") || strings.Contains(lower, "different decision identity") {
		return fmt.Errorf("%w: %s", review.ErrDenConflict, text)
	}
	return fmt.Errorf("Den MCP %s: %s", operation, text)
}

// Context response fields are the subset needed to validate that the bounded
// Den reviewer context belongs to the admitted review. The original bounded
// structuredContent is retained separately as private prompt material.
type contextResponse struct {
	ProjectID    string        `json:"project_id"`
	TaskID       int64         `json:"task_id"`
	Task         contextTask   `json:"task"`
	CurrentRound *contextRound `json:"current_round"`
	NextState    string        `json:"next_state"`
}

type contextTask struct {
	ID               int64  `json:"id"`
	ProjectID        string `json:"project_id"`
	RootPath         string `json:"root_path"`
	RepositoryHandle string `json:"repository_handle"`
}

type contextRound struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	TaskID    int64  `json:"task_id"`
}

// GetReviewContext maps Den's bounded task-scoped context to the runner's
// identity. Project/task/round identities are checked against the admitted
// key; the correlation ID is local admission identity and is retained by the
// caller rather than fabricated into Den's task context.
func (c *Client) GetReviewContext(ctx context.Context, key review.Key) (review.Context, error) {
	data, err := c.call(ctx, "get_review_context", map[string]any{"task_id": key.TaskID})
	if err != nil {
		return review.Context{}, err
	}
	var response contextResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.Context{}, fmt.Errorf("decode Den review context: %w", err)
	}
	if response.ProjectID == "" {
		response.ProjectID = response.Task.ProjectID
	}
	if response.TaskID == 0 {
		response.TaskID = response.Task.ID
	}
	if response.ProjectID != key.ProjectID || response.TaskID != key.TaskID {
		return review.Context{}, fmt.Errorf("%w: Den context identity is %s/%d, admitted key is %s/%d", review.ErrStaleRound, response.ProjectID, response.TaskID, key.ProjectID, key.TaskID)
	}
	if response.CurrentRound == nil {
		return review.Context{}, fmt.Errorf("%w: Den context has no current review round", review.ErrStaleRound)
	}
	if response.CurrentRound.ID != key.ReviewRoundID || response.CurrentRound.ProjectID != "" && response.CurrentRound.ProjectID != key.ProjectID || response.CurrentRound.TaskID != 0 && response.CurrentRound.TaskID != key.TaskID {
		return review.Context{}, fmt.Errorf("%w: Den context round %d does not match admitted round %d", review.ErrStaleRound, response.CurrentRound.ID, key.ReviewRoundID)
	}
	workspace := strings.TrimSpace(response.Task.RootPath)
	if workspace == "" {
		workspace = strings.TrimSpace(response.Task.RepositoryHandle)
	}
	return review.Context{
		Key:       key,
		NextState: strings.TrimSpace(response.NextState),
		Workspace: workspace,
		Material:  append(json.RawMessage(nil), data...),
	}, nil
}

type requestReviewResponse struct {
	ID            int64  `json:"id"`
	ProjectID     string `json:"project_id"`
	TaskID        int64  `json:"task_id"`
	ReviewRoundID *int64 `json:"review_round_id"`
}

// RequestReview starts or reuses Den's current review round. Source revision
// evidence remains in the Den request notes/gate; this adapter does not copy a
// second review lifecycle into crew-services.
func (c *Client) RequestReview(ctx context.Context, request review.SubmissionRequest) (review.ReviewRoundRef, error) {
	data, err := c.call(ctx, "request_review", map[string]any{
		"task_id":      request.TaskID,
		"requested_by": request.Reviewer,
		"branch":       request.Ref,
		"base_branch":  request.Ref,
		"tests_run":    []string{},
		"notes":        reviewRequestNotes(request),
	})
	if err != nil {
		return review.ReviewRoundRef{}, err
	}
	var response requestReviewResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.ReviewRoundRef{}, fmt.Errorf("decode Den review request: %w", err)
	}
	roundID := response.ID
	if response.ReviewRoundID != nil {
		roundID = *response.ReviewRoundID
	}
	if roundID <= 0 {
		return review.ReviewRoundRef{}, errors.New("Den request_review returned no review round id")
	}
	if response.ProjectID != "" && response.ProjectID != request.ProjectID || response.TaskID != 0 && response.TaskID != request.TaskID {
		return review.ReviewRoundRef{}, fmt.Errorf("%w: Den request_review returned %s/%d", review.ErrDenConflict, response.ProjectID, response.TaskID)
	}
	return review.ReviewRoundRef{ID: roundID, ProjectID: request.ProjectID, TaskID: request.TaskID}, nil
}

type gateResponse struct {
	ID             int64    `json:"id"`
	GateID         int64    `json:"gate_id"`
	ProjectID      string   `json:"project_id"`
	TaskID         int64    `json:"task_id"`
	Repository     string   `json:"repository"`
	CommitSHA      string   `json:"commit_sha"`
	Ref            string   `json:"ref"`
	Status         string   `json:"status"`
	TerminalReason string   `json:"terminal_reason"`
	FailureSummary string   `json:"failure_summary"`
	RequiredChecks []string `json:"required_checks"`
}

func (c *Client) WatchGitHubChecks(ctx context.Context, request review.GateRequest) (review.GateEvidence, error) {
	data, err := c.call(ctx, "watch_github_checks", map[string]any{
		"task_id":         request.TaskID,
		"repository":      request.Repository,
		"commit_sha":      request.CommitSHA,
		"ref":             request.Ref,
		"required_checks": request.RequiredChecks,
		"requested_by":    request.RequestedBy,
	})
	if err != nil {
		return review.GateEvidence{}, err
	}
	return parseGateResponse(data, request)
}

func (c *Client) GetGitHubCheckGate(ctx context.Context, request review.GateRequest) (review.GateEvidence, error) {
	data, err := c.call(ctx, "get_github_check_gate", map[string]any{
		"task_id":    request.TaskID,
		"commit_sha": request.CommitSHA,
	})
	if err != nil {
		return review.GateEvidence{}, err
	}
	return parseGateResponse(data, request)
}

func parseGateResponse(data json.RawMessage, request review.GateRequest) (review.GateEvidence, error) {
	var response gateResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.GateEvidence{}, fmt.Errorf("decode Den GitHub check gate: %w", err)
	}
	gateID := response.ID
	if response.GateID != 0 {
		gateID = response.GateID
	}
	if gateID <= 0 || strings.TrimSpace(response.Status) == "" {
		return review.GateEvidence{}, errors.New("Den GitHub check gate response omitted id or status")
	}
	if response.ProjectID != "" && response.ProjectID != request.ProjectID || response.TaskID != 0 && response.TaskID != request.TaskID || response.Repository != "" && response.Repository != request.Repository || response.CommitSHA != "" && !strings.EqualFold(response.CommitSHA, request.CommitSHA) {
		return review.GateEvidence{}, fmt.Errorf("%w: Den GitHub check gate identity does not match submission", review.ErrDenConflict)
	}
	return review.GateEvidence{
		Repository:     request.Repository,
		Ref:            request.Ref,
		CommitSHA:      request.CommitSHA,
		Status:         strings.TrimSpace(response.Status),
		Handle:         strconv.FormatInt(gateID, 10),
		TerminalReason: strings.TrimSpace(response.TerminalReason),
		FailureSummary: strings.TrimSpace(response.FailureSummary),
	}, nil
}

func reviewRequestNotes(request review.SubmissionRequest) string {
	notes := request.ReviewSummary
	notes += "\n\nManaged submission source: " + request.Repository + " @ " + request.CommitSHA + " (" + request.Ref + ")."
	if request.BaseCommit != "" {
		notes += " Diff base: " + request.BaseCommit + "."
	}
	return notes
}

type finalizeResponse struct {
	Schema              string `json:"schema"`
	SchemaVersion       int    `json:"schema_version"`
	ID                  int64  `json:"id"`
	ProjectID           string `json:"project_id"`
	TaskID              int64  `json:"task_id"`
	ReviewRoundID       int64  `json:"review_round_id"`
	Verdict             string `json:"verdict"`
	TargetTaskStatus    string `json:"target_task_status"`
	State               string `json:"state"`
	PacketID            int64  `json:"packet_id"`
	PacketMessageID     *int64 `json:"packet_message_id"`
	MessageID           *int64 `json:"message_id"`
	ResultingTaskStatus string `json:"resulting_task_status"`
	LastErrorStep       string `json:"last_error_step"`
	LastError           string `json:"last_error"`
}

// FinalizeReview submits only the controller-owned Den finalization shape and
// converts its typed receipt into the local job receipt. Den's idempotent
// finalization operation is the completion authority.
func (c *Client) FinalizeReview(ctx context.Context, finalization review.Finalization) (review.Receipt, error) {
	notes := strings.TrimSpace(finalization.Completion.Notes)
	if evidence := strings.TrimSpace(finalization.Completion.Evidence); evidence != "" {
		if notes != "" {
			notes += "\n\n"
		}
		notes += "Evidence:\n" + evidence
	}
	arguments := map[string]any{
		"review_round_id": finalization.Key.ReviewRoundID,
		"verdict":         finalization.Completion.Verdict,
		"decided_by":      finalization.Reviewer,
	}
	if notes != "" {
		arguments["notes"] = notes
	}
	if len(finalization.Completion.PriorResolutions) > 0 {
		arguments["prior_finding_resolutions"] = finalization.Completion.PriorResolutions
	}
	if len(finalization.Completion.NewFindings) > 0 {
		arguments["new_findings"] = finalization.Completion.NewFindings
	}
	data, err := c.call(ctx, "finalize_review", arguments)
	if err != nil {
		return review.Receipt{}, err
	}
	var response finalizeResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.Receipt{}, fmt.Errorf("decode Den finalization receipt: %w", err)
	}
	if response.ProjectID != "" && response.ProjectID != finalization.Key.ProjectID || response.TaskID != 0 && response.TaskID != finalization.Key.TaskID || response.ReviewRoundID != 0 && response.ReviewRoundID != finalization.Key.ReviewRoundID {
		return review.Receipt{}, fmt.Errorf("%w: Den finalization identity does not match admitted key", review.ErrDenConflict)
	}
	if response.Verdict != "" && response.Verdict != finalization.Completion.Verdict {
		return review.Receipt{}, fmt.Errorf("%w: Den returned verdict %q for %q", review.ErrDenConflict, response.Verdict, finalization.Completion.Verdict)
	}
	resultingStatus := response.ResultingTaskStatus
	if resultingStatus == "" {
		resultingStatus = response.TargetTaskStatus
	}
	return review.Receipt{
		Schema:              response.Schema,
		SchemaVersion:       response.SchemaVersion,
		FinalizationID:      response.ID,
		ProjectID:           first(response.ProjectID, finalization.Key.ProjectID),
		TaskID:              firstInt(response.TaskID, finalization.Key.TaskID),
		ReviewRoundID:       firstInt(response.ReviewRoundID, finalization.Key.ReviewRoundID),
		Verdict:             first(response.Verdict, finalization.Completion.Verdict),
		State:               response.State,
		ResultingTaskStatus: resultingStatus,
		PacketID:            response.PacketID,
		PacketMessageID:     response.PacketMessageID,
		MessageID:           response.MessageID,
		LastErrorStep:       response.LastErrorStep,
		LastError:           response.LastError,
	}, nil
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstInt(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func boundedText(data []byte, limit int) string {
	text := strings.TrimSpace(string(data))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
