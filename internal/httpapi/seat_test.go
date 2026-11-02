package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

func TestSeatRoutesTakeRefuseAndList(t *testing.T) {
	ctx := context.Background()
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "seats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	svc, err := service.New(persistence, fixedClock{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(svc)
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		} else {
			reader = bytes.NewReader(nil)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, reader))
		var decoded map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
		return recorder.Code, decoded
	}
	sessions := map[string]string{}
	for _, adapter := range []string{"crew-claude", "crew-codex"} {
		lease, err := svc.RegisterAdapter(ctx, service.RegisterAdapterRequest{AdapterID: adapter, InstanceID: adapter + "-1", LeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		binding, err := svc.RegisterSessionAddress(ctx, service.RegisterSessionAddressRequest{AdapterID: adapter, LeaseToken: lease.LeaseToken, NativeKey: "native"})
		if err != nil {
			t.Fatal(err)
		}
		sessions[adapter] = binding.Address
		sessions[adapter+"/token"] = lease.LeaseToken
	}
	take := func(adapter, op string) (int, map[string]any) {
		return do(http.MethodPost, "/v1/seats/rusty-engine.lead/take", map[string]any{"adapter_id": adapter, "lease_token": sessions[adapter+"/token"], "operation_id": op, "session_address": sessions[adapter]})
	}
	if status, body := take("crew-claude", "take-1"); status != http.StatusCreated || body["holding"].(map[string]any)["holder_address"] != sessions["crew-claude"] {
		t.Fatalf("take = %d %v", status, body)
	}
	status, body := take("crew-codex", "take-2")
	if status != http.StatusConflict || body["code"] != "seat_held" {
		t.Fatalf("second take = %d %v", status, body)
	}
	if holder := body["detail"].(map[string]any)["holding"].(map[string]any)["holder_address"]; holder != sessions["crew-claude"] {
		t.Fatalf("seat_held names %v, want %s", holder, sessions["crew-claude"])
	}
	if status, body := do(http.MethodPut, "/v1/seats/rusty-engine.lead/charter", map[string]any{"adapter_id": "crew-codex", "lease_token": sessions["crew-codex/token"], "operation_id": "charter-1", "session_address": sessions["crew-codex"], "body": "Own the roadmap.\nSay no often."}); status != http.StatusCreated || body["text"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("set charter = %d %v", status, body)
	}
	status, body = do(http.MethodGet, "/v1/seats?scope=rusty-engine", nil)
	seats, _ := body["seats"].([]any)
	if status != http.StatusOK || len(seats) != 1 || seats[0].(map[string]any)["charter_summary"] != "Own the roadmap." {
		t.Fatalf("list = %d %v", status, body)
	}
	if status, body := do(http.MethodGet, "/v1/seats/rusty-engine.lead/charter?version=1", nil); status != http.StatusOK || body["body"] != "Own the roadmap.\nSay no often." {
		t.Fatalf("charter v1 = %d %v", status, body)
	}
	if status, body := do(http.MethodGet, "/v1/seats/rusty-engine.nobody", nil); status != http.StatusNotFound || body["code"] != "seat_not_found" {
		t.Fatalf("missing seat = %d %v", status, body)
	}
}
