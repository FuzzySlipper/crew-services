package codexadapter

import (
	"context"
	"errors"
)

// The methods in this file drive user threads that live in the shared daemon.
// None of them starts, interrupts or unloads a thread.

// LoadedThreads lists the ids of threads the daemon has in memory.
func (c *StdioAppServer) LoadedThreads(ctx context.Context) ([]string, error) {
	if err := c.awaitInitialized(ctx); err != nil {
		return nil, err
	}
	var ids []string
	var cursor *string
	for {
		var response struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.sendRequest(ctx, "thread/loaded/list", params, &response); err != nil {
			return nil, err
		}
		ids = append(ids, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return ids, nil
		}
		cursor = response.NextCursor
	}
}

// ReadThread reads one thread's metadata and status, without turns.
func (c *StdioAppServer) ReadThread(ctx context.Context, threadID string) (NativeThread, error) {
	var response struct {
		Thread nativeThreadWire `json:"thread"`
	}
	if err := c.awaitInitialized(ctx); err != nil {
		return NativeThread{}, err
	}
	if err := c.sendRequest(ctx, "thread/read", map[string]any{"threadId": threadID}, &response); err != nil {
		return NativeThread{}, err
	}
	return response.Thread.native(), nil
}

// NativeTurn is one turn from thread/turns/list. ClientIDs holds the
// clientUserMessageId of every user message in it, when items were loaded.
type NativeTurn struct {
	ID        string
	Status    string
	ClientIDs []string
}

// RecentTurns lists the thread's newest turns first. withItems loads the
// turns' items so their user messages' client ids can be checked.
func (c *StdioAppServer) RecentTurns(ctx context.Context, threadID string, limit int, withItems bool) ([]NativeTurn, error) {
	if err := c.awaitInitialized(ctx); err != nil {
		return nil, err
	}
	view := "notLoaded"
	if withItems {
		view = "full"
	}
	var response struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Items  []struct {
				Type     string  `json:"type"`
				ClientID *string `json:"clientId"`
			} `json:"items"`
		} `json:"data"`
	}
	params := map[string]any{"threadId": threadID, "limit": limit, "sortDirection": "desc", "itemsView": view}
	if err := c.sendRequest(ctx, "thread/turns/list", params, &response); err != nil {
		return nil, err
	}
	turns := make([]NativeTurn, 0, len(response.Data))
	for _, turn := range response.Data {
		value := NativeTurn{ID: turn.ID, Status: turn.Status}
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID != "" {
				value.ClientIDs = append(value.ClientIDs, *item.ClientID)
			}
		}
		turns = append(turns, value)
	}
	return turns, nil
}

// ActiveTurn returns the id of the thread's in-progress turn, or "".
func (c *StdioAppServer) ActiveTurn(ctx context.Context, threadID string) (string, error) {
	turns, err := c.RecentTurns(ctx, threadID, 1, false)
	if err != nil {
		return "", err
	}
	if len(turns) == 1 && turns[0].Status == "inProgress" {
		return turns[0].ID, nil
	}
	return "", nil
}

// Steer adds text to the exact active turn. Codex records it as a user
// message at once and the model reads it before its next request; nothing is
// cancelled. It fails when that turn is no longer the active one.
func (c *StdioAppServer) Steer(ctx context.Context, threadID, turnID, text, clientUserMessageID string) error {
	if err := c.awaitInitialized(ctx); err != nil {
		return err
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	params := map[string]any{
		"threadId": threadID, "expectedTurnId": turnID, "clientUserMessageId": clientUserMessageID,
		"input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}},
	}
	return c.sendRequest(ctx, "turn/steer", params, &response)
}

// Resume loads a thread that is not loaded, so the daemon starts its queued
// items. This connection becomes one of the thread's subscribers.
func (c *StdioAppServer) Resume(ctx context.Context, threadID string) error {
	if err := c.awaitInitialized(ctx); err != nil {
		return err
	}
	var ignored map[string]any
	return c.sendRequest(ctx, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &ignored)
}

// IsRejection reports whether err is the app-server's own answer refusing a
// request, as opposed to a lost connection or timeout. A refused request had
// no effect.
func IsRejection(err error) bool {
	var rpc *rpcError
	return errors.As(err, &rpc)
}
