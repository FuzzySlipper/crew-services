package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// InteractionOptions deliberately permits only the product's two read-only
// query commands, never an arbitrary debug command or an activation operation.
type InteractionOptions struct {
	Mode   string   `json:"mode,omitempty"`
	X      *float64 `json:"x,omitempty"`
	Y      *float64 `json:"y,omitempty"`
	Aspect *float64 `json:"aspect,omitempty"`
}

func (s *Service) Interaction(ctx context.Context, id string, data json.RawMessage) (any, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	err := s.available(id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.interactionCall(ctx, id, data)
}

func (s *Service) interactionCall(ctx context.Context, id string, data json.RawMessage) (any, error) {
	var o InteractionOptions
	if len(data) > 0 {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&o); err != nil {
			return nil, err
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("expected one interaction options object")
		}
	}
	if o.Mode == "" {
		o.Mode = "reticle"
	}
	command := "interaction.query"
	switch o.Mode {
	case "reticle":
		if o.X != nil || o.Y != nil || o.Aspect != nil {
			return nil, errors.New("reticle queries do not accept cursor coordinates")
		}
	case "cursor":
		if o.X == nil || o.Y == nil || o.Aspect == nil || !finite(*o.X) || !finite(*o.Y) || !finite(*o.Aspect) || *o.X < 0 || *o.X > 1 || *o.Y < 0 || *o.Y > 1 || *o.Aspect <= 0 {
			return nil, errors.New("cursor requires normalized bottom-left x/y in [0,1] and positive finite aspect")
		}
		command = fmt.Sprintf("interaction.cursor %g %g %g", *o.X, *o.Y, *o.Aspect)
	default:
		return nil, errors.New("interaction mode must be reticle or cursor")
	}
	s.mu.Lock()
	st, err := s.require(id)
	if err == nil && (s.current != id || st.Phase != "connected") {
		err = errors.New("session is no longer connected; recover before querying")
	}
	var p Profile
	if err == nil {
		p, err = s.profile(st.Game)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !p.InteractionQueries {
		return nil, errors.New("capability_unavailable: profile has not opted into product interaction queries")
	}
	return s.debugQuery(ctx, id, p.URL, command, "interaction", "product live-debug query; semantic assistance")
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
