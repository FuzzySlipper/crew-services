package wolf

import (
	"context"
	"errors"
	"testing"
)

func TestPointerLockDiagnosticIsUnknownAndFresh(t *testing.T) {
	first := PointerLockDiagnostic()
	second := PointerLockDiagnostic()
	if first["state"] != pointerLockUnknown || first["observability"] != pointerLockUnavailable {
		t.Fatalf("pointer lock diagnostic = %#v", first)
	}
	first["state"] = "locked"
	if second["state"] != pointerLockUnknown {
		t.Fatalf("pointer lock diagnostic was shared: %#v", second)
	}
}

func TestWithInputDiagnosticsSeparatesTargetDeliveryFromGameConsumption(t *testing.T) {
	original := map[string]any{
		"backend":         "wolf-input-api",
		"steps":           []map[string]any{{"kind": "move"}, {"kind": "click"}},
		"completed_steps": float64(2),
	}
	annotated := WithInputDiagnostics(original)
	delivery, ok := annotated["input_delivery"].(map[string]any)
	if !ok {
		t.Fatalf("input delivery diagnostic missing: %#v", annotated)
	}
	if delivery["state"] != inputDeliveryTargetReport || delivery["requested_steps"] != 2 || delivery["completed_steps"] != 2 {
		t.Fatalf("input delivery diagnostic = %#v", delivery)
	}
	if delivery["game_consumption"] != gameConsumptionUnknown {
		t.Fatalf("game consumption was fabricated: %#v", delivery)
	}
	lock, ok := annotated["pointer_lock"].(map[string]any)
	if !ok || lock["state"] != pointerLockUnknown {
		t.Fatalf("pointer lock diagnostic = %#v", lock)
	}
	if _, changed := original["input_delivery"]; changed {
		t.Fatalf("input diagnostics mutated remote receipt: %#v", original)
	}
}

func TestWithInputDiagnosticsDoesNotTreatPartialOrErrorReceiptAsDelivered(t *testing.T) {
	for _, receipt := range []map[string]any{
		{"steps": []any{map[string]any{"kind": "move"}, map[string]any{"kind": "click"}}, "completed_steps": 1},
		{"steps": []any{map[string]any{"kind": "move"}}, "completed_steps": 1, "error": "Wolf sessions/input timed out"},
		{"steps": []any{map[string]any{"kind": "move"}}},
		{"steps": []any{}, "completed_steps": 0},
		{"steps": []any{map[string]any{"kind": "move"}}, "completed_steps": 1, "cancelled": true},
		{"steps": []any{map[string]any{"kind": "move"}}, "completed_steps": 1, "release_errors": []any{"key up failed"}},
		{"steps": []any{map[string]any{"kind": "move"}}, "completed_steps": 1, "evidence_error": "journal failed"},
		{"steps": []any{map[string]any{"kind": "move"}}, "completed_steps": -1},
	} {
		annotated := WithInputDiagnostics(receipt)
		delivery := annotated["input_delivery"].(map[string]any)
		if delivery["state"] != inputDeliveryUnknown {
			t.Fatalf("unsafe delivery claim for %#v: %#v", receipt, delivery)
		}
	}
}

func TestWithInputDiagnosticsReportsCancellationWithoutClaimingDelivery(t *testing.T) {
	annotated := WithInputDiagnostics(map[string]any{
		"steps": []any{map[string]any{"kind": "hold"}}, "completed_steps": 1, "cancelled": true,
	})
	delivery := annotated["input_delivery"].(map[string]any)
	if delivery["state"] != inputDeliveryUnknown || delivery["outcome"] != "cancelled" {
		t.Fatalf("cancelled delivery diagnostic = %#v", delivery)
	}
}

func TestWithStatusDiagnosticsPreservesStatusAndAnnotatesLastReceipt(t *testing.T) {
	status := map[string]any{
		"lease": nil, "sessions": []any{},
		"last_input": map[string]any{"steps": []any{map[string]any{"kind": "click"}}, "completed_steps": 1},
	}
	annotated := WithStatusDiagnostics(status)
	if annotated["lease"] != nil || len(annotated["sessions"].([]any)) != 0 {
		t.Fatalf("status changed: %#v", annotated)
	}
	if _, changed := status["pointer_lock"]; changed {
		t.Fatalf("status was mutated: %#v", status)
	}
	lastInput := annotated["last_input"].(map[string]any)
	if lastInput["input_delivery"].(map[string]any)["state"] != inputDeliveryTargetReport {
		t.Fatalf("last receipt was not annotated: %#v", lastInput)
	}
}

func TestAdapterPublishesDiagnosticsWithInputAndStatusReceipts(t *testing.T) {
	adapter := &Adapter{
		lease:     map[string]any{"id": "lease-1", "client_id": "client-1"},
		directory: t.TempDir(),
		remoteCall: func(_ context.Context, operation string, _ map[string]any) (map[string]any, error) {
			switch operation {
			case "input":
				return map[string]any{"steps": []any{map[string]any{"kind": "click"}}, "completed_steps": 1}, nil
			case "status":
				return map[string]any{"last_input": map[string]any{"steps": []any{map[string]any{"kind": "click"}}, "completed_steps": 1}}, nil
			default:
				return nil, errors.New("unexpected target operation")
			}
		},
	}
	receipt, err := adapter.Input(context.Background(), "lease-1", []map[string]any{{"kind": "click"}})
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if receipt["pointer_lock"].(map[string]any)["state"] != pointerLockUnknown {
		t.Fatalf("input lock state = %#v", receipt)
	}
	if receipt["input_delivery"].(map[string]any)["state"] != inputDeliveryTargetReport {
		t.Fatalf("input delivery = %#v", receipt)
	}
	status, err := adapter.Status(context.Background(), "lease-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status["pointer_lock"].(map[string]any)["state"] != pointerLockUnknown {
		t.Fatalf("status lock state = %#v", status)
	}
	lastInput := status["last_input"].(map[string]any)
	if lastInput["input_delivery"].(map[string]any)["game_consumption"] != gameConsumptionUnknown {
		t.Fatalf("status game consumption = %#v", lastInput)
	}
}
