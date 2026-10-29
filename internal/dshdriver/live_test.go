//go:build dshlive

package dshdriver

// Live check against the running `dsh web` host: a driver retry after an
// ambiguous prompt outcome produces exactly one turn. Run it only against a
// scratch session:
//
//	DSH_LIVE_SESSION=session-... go test -tags dshlive -run TestLive -v ./internal/dshdriver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// lossy forwards to the real host but makes the first prompt's outcome
// ambiguous: "lose-response" sends it and reports a reset connection at once,
// inside the inbox-to-history window; "lose-request" reports the same error
// without sending.
type lossy struct {
	Native
	mode  string
	calls int
	sent  []time.Time
}

func (l *lossy) Prompt(ctx context.Context, p PromptRequest) error {
	l.calls++
	if l.calls == 1 {
		if l.mode == "lose-response" {
			if err := l.Native.Prompt(ctx, p); err != nil {
				return err
			}
			l.sent = append(l.sent, time.Now())
		}
		return errors.New("dsh session/prompt: connection reset by peer (simulated)")
	}
	l.sent = append(l.sent, time.Now())
	return l.Native.Prompt(ctx, p)
}

func TestLiveRetryAfterAmbiguousPromptRunsOneTurn(t *testing.T) {
	session := os.Getenv("DSH_LIVE_SESSION")
	if session == "" {
		t.Skip("DSH_LIVE_SESSION is not set")
	}
	for _, mode := range []string{"lose-response", "lose-request"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			host := NewWebHost(JournalTokens("dsh-web.service", ""))
			before, err := host.Events(ctx, session, 400)
			if err != nil {
				t.Fatal(err)
			}
			native := &lossy{Native: host, mode: mode}
			fabric := newFakeFabric()
			cfg := Defaults()
			d := NewDriver(fabric, native, time.Now, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := d.Start(ctx); err != nil {
				t.Fatal(err)
			}
			deliveryID := "live-" + mode + "-" + time.Now().UTC().Format("150405.000")
			sessions, err := host.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range sessions {
				if s.SessionID == session {
					if _, err := d.register(ctx, s); err != nil {
						t.Fatal(err)
					}
				}
			}
			address := "s/" + session
			claim := claimFor(address, deliveryID, "deliver_at_idle", "next_turn")
			claim.Message.Body = "Live retry check (" + mode + "). Reply with the single word ONCE. Do not use tools and do not send a crew reply."
			fabric.queue(address, claim)
			start := time.Now()
			if err := d.Pump(ctx); err != nil {
				t.Fatal(err)
			}
			t.Logf("prompt calls %d, sent at %v, settled after %v: acked %v unknown %v requeued %d", native.calls, native.sent, time.Since(start), fabric.acked, fabric.unknown, len(fabric.requeued))
			// Let the turn finish, then count what the session did with it.
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				state, err := host.State(ctx, session)
				if err == nil && !state.Running && len(state.Queue) == 0 {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			after, err := host.Events(ctx, session, 400)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[int64]bool{}
			for _, e := range before {
				seen[e.Seq] = true
			}
			turns, messages := 0, 0
			for _, e := range after {
				if seen[e.Seq] {
					continue
				}
				switch e.Type {
				case "turn/start":
					turns++
				case "user/message":
					var data struct {
						Source struct {
							RPCID string `json:"rpcId"`
						} `json:"source"`
					}
					if json.Unmarshal(e.Data, &data) == nil && data.Source.RPCID == RequestID(deliveryID) {
						messages++
					}
				}
				t.Logf("event %d %s", e.Seq, e.Type)
			}
			if len(fabric.acked) != 1 || turns != 1 || messages != 1 {
				t.Fatalf("acked %v, %d new turns, %d user messages with the request id; want 1, 1, 1", fabric.acked, turns, messages)
			}
		})
	}
}
