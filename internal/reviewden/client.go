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
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"crew-services/internal/review"
)

const (
	DefaultMCPURL               = "http://192.168.1.10:5199/mcp"
	maxResponseBytes            = 1 << 20
	maxFinalizationRequestBytes = 16 * 1024
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
var _ review.ManualReviewDenClient = (*Client)(nil)

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
	if strings.TrimSpace(strings.ToLower(code)) == "task_not_reviewable" {
		return fmt.Errorf("%w: %s", review.ErrTaskNotReviewable, text)
	}
	if strings.Contains(lower, "stale_review_round") || strings.Contains(lower, "review round is no longer current") || strings.Contains(lower, "round is stale") {
		return fmt.Errorf("%w: %s", review.ErrStaleRound, text)
	}
	if strings.Contains(lower, "review_finalization_conflict") || strings.Contains(lower, "finalization conflict") || strings.Contains(lower, "different decision identity") {
		return fmt.Errorf("%w: %s", review.ErrDenConflict, text)
	}
	if permanentFinalizationCode(strings.ToLower(strings.TrimSpace(code))) ||
		strings.Contains(lower, "review_request_too_large") ||
		strings.Contains(lower, "unresolved_review_findings") ||
		strings.Contains(lower, "actionable_review_finding_required") {
		return fmt.Errorf("%w: %s", review.ErrDenRejected, text)
	}
	return fmt.Errorf("Den MCP %s: %s", operation, text)
}

func permanentFinalizationCode(code string) bool {
	return code == "review_request_too_large" ||
		strings.HasPrefix(code, "invalid_") ||
		strings.HasPrefix(code, "missing_") ||
		code == "unresolved_findings" ||
		code == "unresolved_review_findings" ||
		code == "actionable_finding" ||
		code == "actionable_review_finding_required" ||
		code == "task_not_reviewable"
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

type taskContextResponse struct {
	ProjectID      string              `json:"project_id"`
	TaskID         int64               `json:"task_id"`
	Task           json.RawMessage     `json:"task"`
	RecentMessages json.RawMessage     `json:"recent_messages"`
	Workflow       taskContextWorkflow `json:"workflow"`
}

type taskContextTask struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
}

type taskContextWorkflow struct {
	CurrentReviewRound json.RawMessage `json:"current_review_round"`
}

type taskContextReviewRound struct {
	ID int64 `json:"id"`
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
	if workspace == "" || !filepath.IsAbs(workspace) {
		return review.Context{}, fmt.Errorf("%w; set Den project root_path (got %q)", review.ErrWorkspaceRequired, response.Task.RootPath)
	}
	taskData, err := c.call(ctx, "get_task_context", map[string]any{"task_id": key.TaskID})
	if err != nil {
		return review.Context{}, err
	}
	if _, _, _, err := decodeTaskContext(taskData, key.Task()); err != nil {
		return review.Context{}, err
	}
	material, err := mergeTaskContext(data, taskData, key.Task())
	if err != nil {
		return review.Context{}, err
	}
	return review.Context{
		Key:       key,
		NextState: strings.TrimSpace(response.NextState),
		Workspace: workspace,
		Material:  material,
	}, nil
}

// GetTaskContext reads the canonical task context for manual-review
// authorization. Project scope is still derived from the requested path by
// the caller; the task itself remains the authority for status.
func (c *Client) GetTaskContext(ctx context.Context, key review.TaskKey) (review.TaskContext, error) {
	data, err := c.call(ctx, "get_task_context", map[string]any{"task_id": key.TaskID})
	if err != nil {
		return review.TaskContext{}, err
	}
	task, _, _, err := decodeTaskContext(data, key)
	if err != nil {
		return review.TaskContext{}, err
	}
	return task, nil
}

func decodeTaskContext(data json.RawMessage, key review.TaskKey) (review.TaskContext, json.RawMessage, json.RawMessage, error) {
	var response taskContextResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.TaskContext{}, nil, nil, fmt.Errorf("decode Den task context: %w", err)
	}
	var task taskContextTask
	if len(response.Task) > 0 && string(response.Task) != "null" {
		if err := json.Unmarshal(response.Task, &task); err != nil {
			return review.TaskContext{}, nil, nil, fmt.Errorf("decode Den task identity: %w", err)
		}
	}
	projectID := strings.TrimSpace(response.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(task.ProjectID)
	}
	taskID := response.TaskID
	if taskID == 0 {
		taskID = task.ID
	}
	if projectID != key.ProjectID || taskID != key.TaskID || task.ProjectID != "" && strings.TrimSpace(task.ProjectID) != key.ProjectID || task.ID != 0 && task.ID != key.TaskID {
		return review.TaskContext{}, nil, nil, fmt.Errorf("%w: Den task context identity is %s/%d, requested %s/%d", review.ErrConflict, projectID, taskID, key.ProjectID, key.TaskID)
	}
	currentRoundID, err := taskContextCurrentReviewRoundID(response.Workflow.CurrentReviewRound)
	if err != nil {
		return review.TaskContext{}, nil, nil, err
	}
	return review.TaskContext{ProjectID: projectID, TaskID: taskID, Status: strings.TrimSpace(task.Status), CurrentReviewRoundID: currentRoundID}, append(json.RawMessage(nil), response.Task...), append(json.RawMessage(nil), response.RecentMessages...), nil
}

func taskContextCurrentReviewRoundID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var round taskContextReviewRound
	if err := json.Unmarshal(raw, &round); err != nil {
		return 0, fmt.Errorf("decode Den current review round: %w", err)
	}
	if round.ID < 0 {
		return 0, errors.New("Den current review round id must not be negative")
	}
	return round.ID, nil
}

func mergeTaskContext(reviewData, taskData json.RawMessage, key review.TaskKey) (json.RawMessage, error) {
	_, taskRaw, messagesRaw, err := decodeTaskContext(taskData, key)
	if err != nil {
		return nil, err
	}
	var reviewObject map[string]json.RawMessage
	if err := json.Unmarshal(reviewData, &reviewObject); err != nil {
		return nil, fmt.Errorf("decode Den review context for task merge: %w", err)
	}
	var reviewTask map[string]json.RawMessage
	if raw := reviewObject["task"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &reviewTask); err != nil {
			return nil, fmt.Errorf("decode Den review task for task merge: %w", err)
		}
	} else {
		reviewTask = make(map[string]json.RawMessage)
	}
	var canonicalTask map[string]json.RawMessage
	if len(taskRaw) > 0 && string(taskRaw) != "null" {
		if err := json.Unmarshal(taskRaw, &canonicalTask); err != nil {
			return nil, fmt.Errorf("decode Den canonical task for task merge: %w", err)
		}
	}
	for field, value := range canonicalTask {
		// The canonical task context is the authority for user-facing task
		// content and status. The review context contributes only review
		// infrastructure fields such as root_path and repository_handle.
		if field == "description" || field == "title" || field == "status" || field == "id" || field == "project_id" {
			reviewTask[field] = value
		} else if _, exists := reviewTask[field]; !exists {
			reviewTask[field] = value
		}
	}
	encodedTask, err := json.Marshal(reviewTask)
	if err != nil {
		return nil, fmt.Errorf("encode merged Den task context: %w", err)
	}
	reviewObject["task"] = encodedTask
	if len(messagesRaw) > 0 && string(messagesRaw) != "null" {
		reviewObject["recent_messages"] = messagesRaw
	}
	encoded, err := json.Marshal(reviewObject)
	if err != nil {
		return nil, fmt.Errorf("encode merged Den review context: %w", err)
	}
	return encoded, nil
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

// RequestManualReview creates or reuses the Den round using only the
// task-scoped request fields. In particular, it never manufactures a commit
// SHA or submits a source-control gate for a best-effort review.
func (c *Client) RequestManualReview(ctx context.Context, request review.ManualReviewRequest) (review.ReviewRoundRef, error) {
	data, err := c.call(ctx, "request_review", map[string]any{
		"task_id":      request.TaskID,
		"requested_by": request.Reviewer,
		"branch":       request.Ref,
		"base_branch":  request.Ref,
		"tests_run":    []string{},
		"notes":        request.Preamble,
	})
	if err != nil {
		return review.ReviewRoundRef{}, err
	}
	var response requestReviewResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return review.ReviewRoundRef{}, fmt.Errorf("decode Den manual review request: %w", err)
	}
	roundID := response.ID
	if response.ReviewRoundID != nil {
		roundID = *response.ReviewRoundID
	}
	if roundID <= 0 {
		return review.ReviewRoundRef{}, errors.New("Den manual review request returned no review round id")
	}
	if response.ProjectID != "" && response.ProjectID != request.ProjectID || response.TaskID != 0 && response.TaskID != request.TaskID {
		return review.ReviewRoundRef{}, fmt.Errorf("%w: Den manual review request returned %s/%d", review.ErrDenConflict, response.ProjectID, response.TaskID)
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
	if err := c.ValidateFinalization(finalization); err != nil {
		return review.Receipt{}, err
	}
	arguments := finalizationArguments(finalization)
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

// ValidateFinalization mirrors the compact body emitted by Den's MCP Review
// adapter. Go's JSON encoder escapes characters such as '>', so counting raw
// model text is not sufficient to enforce Den's 16 KiB request contract.
func (c *Client) ValidateFinalization(finalization review.Finalization) error {
	if !finalization.Completion.Valid() {
		return fmt.Errorf("%w: invalid finalization verdict/finding combination", review.ErrDenRejected)
	}
	encoded, err := json.Marshal(finalizationArguments(finalization))
	if err != nil {
		return fmt.Errorf("encode Den finalization arguments: %w", err)
	}
	if len(encoded) > maxFinalizationRequestBytes {
		return fmt.Errorf("%w: review_request_too_large: encoded request is %d bytes (maximum %d); shorten notes, evidence, or finding details", review.ErrDenRejected, len(encoded), maxFinalizationRequestBytes)
	}
	return nil
}

func finalizationArguments(finalization review.Finalization) map[string]any {
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
	return arguments
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
