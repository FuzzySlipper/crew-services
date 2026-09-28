package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crew-services/internal/playtest/session"
)

func TestMalformedInputSurvivesHTTPAndDirectPool(t *testing.T) {
	batches := []string{
		`[{"kind":"hold","keys":["W"],"ms":1}]`,
		`[{"kind":"move","dx":1}]`,
		`[{"kind":"point","x":0,"y":0,"width":1}]`,
		`[{"kind":"click","button":4}]`,
		`[{"kind":"wait","ms":-1}]`,
		`[{"kind":"gamepad","lx":2}]`,
		`[{"kind":"unknown"}]`, `[{"ms":1}]`, `[]`,
	}
	for _, mode := range []string{"http", "direct"} {
		t.Run(mode, func(t *testing.T) {
			p, backends, _ := setup(t, time.Second)
			st := start(t, p, "game-a")
			handler := session.CommandHandler(p)
			valid := session.Request{Op: "input", SessionID: st.ID, Steps: []map[string]any{{"kind": "wait", "ms": 1}}}
			for _, batch := range batches {
				var steps []map[string]any
				if err := json.Unmarshal([]byte(batch), &steps); err != nil {
					t.Fatal(err)
				}
				req := session.Request{Op: "input", SessionID: st.ID, Steps: steps}
				before := backends[0].inputs
				if mode == "http" {
					data, _ := json.Marshal(req)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, httptest.NewRequest("POST", "/command", bytes.NewReader(data)))
					if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_input") {
						t.Fatalf("%s: %d %s", batch, w.Code, w.Body.String())
					}
				} else {
					_, err := p.Command(context.Background(), req)
					if err == nil || !strings.Contains(err.Error(), "invalid_input") {
						t.Fatalf("%s: %v", batch, err)
					}
				}
				if backends[0].inputs != before {
					t.Fatal("invalid batch reached backend")
				}
				if p.slots[0].SlotStatus().Phase != "connected" {
					t.Fatal("rejection degraded session")
				}
				if _, err := p.Command(context.Background(), valid); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Command(context.Background(), session.Request{Op: "stop", SessionID: st.ID}); err != nil {
				t.Fatal(err)
			}
			if p.Activity().(map[string]any)["active_count"] != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}
