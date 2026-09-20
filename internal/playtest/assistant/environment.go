package assistant

import (
	"context"
	"crew-services/internal/playtest/client"
	"encoding/json"
)

type SessionEnvironment struct{ Observer }

func (e *SessionEnvironment) Input(ctx context.Context, steps []map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}
	return e.Client.Result(ctx, client.Request{Op: "input", SessionID: e.SessionID, Steps: raw})
}
func (e *SessionEnvironment) Cancel(ctx context.Context) (json.RawMessage, error) {
	return e.Client.Result(ctx, client.Request{Op: "cancel", SessionID: e.SessionID})
}
