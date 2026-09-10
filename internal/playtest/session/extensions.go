package session

import (
	"context"
	"encoding/json"
	"errors"
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
	return b.Browser(ctx, id, data)
}
