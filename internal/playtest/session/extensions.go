package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"crew-services/internal/playtest/evidence"
	"github.com/google/uuid"
)

func (s *Service) Browser(ctx context.Context, id string, data json.RawMessage) (any, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	err := s.available(id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.browserCall(ctx, id, data)
}

func (s *Service) browserCall(ctx context.Context, id string, data json.RawMessage) (map[string]any, error) {
	b, ok := s.backend.(BrowserBackend)
	if !ok {
		return nil, errors.New("capability_unavailable: browser operations")
	}
	result, err := b.Browser(ctx, id, data)
	if err == nil && result != nil {
		s.keepReceipt(id, data, result)
	}
	return result, err
}

// keepReceipt stores an assist operation's complete result, so a compact
// answer (MCP's default) can point at the product's unchanged facts.
func (s *Service) keepReceipt(id string, data json.RawMessage, result map[string]any) {
	var request struct {
		Op      string `json:"op"`
		Request struct {
			Op string `json:"op"`
		} `json:"request"`
	}
	if json.Unmarshal(data, &request) != nil || request.Op != "playtest" {
		return
	}
	op := request.Request.Op
	if op == "" {
		op = "discover"
	}
	directory := filepath.Join(s.stateDir, "receipts", id)
	path := filepath.Join(directory, fmt.Sprintf("%s-%s-%s.json", time.Now().UTC().Format("20060102T150405.000"), op, uuid.NewString()[:8]))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		result["receipt_error"] = err.Error()
		return
	}
	stored := map[string]any{"session_id": id, "recorded_at": time.Now().UTC(), "request": json.RawMessage(data), "result": result}
	if err := evidence.WriteJSONFile(path, stored); err != nil {
		result["receipt_error"] = err.Error()
		return
	}
	result["receipt"] = path
}
