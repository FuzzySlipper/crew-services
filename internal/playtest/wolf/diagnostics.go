package wolf

import "strings"

const (
	pointerLockUnknown        = "unknown"
	pointerLockUnavailable    = "unavailable"
	inputDeliveryUnknown      = "unknown"
	inputDeliveryTargetReport = "target-reported"
	gameConsumptionUnknown    = "unknown"
)

// PointerLockDiagnostic reports the native Wolf adapter's pointer-lock view.
// Wolf exposes stream and input APIs but no browser pointer-lock readback.
func PointerLockDiagnostic() map[string]any {
	return map[string]any{
		"state":         pointerLockUnknown,
		"observability": pointerLockUnavailable,
		"backend":       "wolf-input-api",
		"reason":        "the native Wolf target exposes no browser pointer-lock readback",
	}
}

// WithPointerLockDiagnostic adds this adapter's current pointer-lock view to a
// receipt or status result. It returns a new top-level map for callers that
// retain the remote result.
func WithPointerLockDiagnostic(result map[string]any) map[string]any {
	annotated := cloneDiagnosticResult(result)
	annotated["pointer_lock"] = PointerLockDiagnostic()
	return annotated
}

// WithInputDiagnostics records only what the native target reported about an
// input batch. A completed batch establishes target-reported transport
// delivery; it does not establish browser pointer lock or game consumption.
func WithInputDiagnostics(receipt map[string]any) map[string]any {
	annotated := WithPointerLockDiagnostic(receipt)
	requested := stepCount(annotated["steps"])
	completed, completedKnown := wholeNumber(annotated["completed_steps"])
	state := inputDeliveryUnknown
	outcome := inputOutcome(annotated, requested, completed, completedKnown)
	if outcome == inputDeliveryTargetReport {
		state = inputDeliveryTargetReport
	}
	annotated["input_delivery"] = map[string]any{
		"state":            state,
		"outcome":          outcome,
		"backend":          "wolf-input-api",
		"requested_steps":  requested,
		"completed_steps":  diagnosticNumber(completed, completedKnown),
		"game_consumption": gameConsumptionUnknown,
	}
	return annotated
}

// WithStatusDiagnostics adds the adapter-level lock view and annotates the
// latest target receipt when the target supplied one.
func WithStatusDiagnostics(status map[string]any) map[string]any {
	annotated := WithPointerLockDiagnostic(status)
	if receipt, ok := annotated["last_input"].(map[string]any); ok {
		annotated["last_input"] = WithInputDiagnostics(receipt)
	}
	return annotated
}

func cloneDiagnosticResult(result map[string]any) map[string]any {
	annotated := make(map[string]any, len(result)+2)
	for key, value := range result {
		annotated[key] = value
	}
	return annotated
}

func stepCount(raw any) int {
	switch steps := raw.(type) {
	case []map[string]any:
		return len(steps)
	case []any:
		return len(steps)
	default:
		return 0
	}
}

func wholeNumber(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		integer := int(value)
		return integer, float64(integer) == value
	default:
		return 0, false
	}
}

func diagnosticNumber(value int, known bool) any {
	if !known {
		return nil
	}
	return value
}

func targetError(receipt map[string]any) string {
	value, _ := receipt["error"].(string)
	return strings.TrimSpace(value)
}

func inputOutcome(receipt map[string]any, requested, completed int, completedKnown bool) string {
	if cancelled, _ := receipt["cancelled"].(bool); cancelled {
		return "cancelled"
	}
	if receiptProblems(receipt, "release_errors") {
		return "cleanup-uncertain"
	}
	if strings.TrimSpace(stringField(receipt, "evidence_error")) != "" {
		return "evidence-uncertain"
	}
	if targetError(receipt) != "" {
		return "target-error"
	}
	if requested > 0 && completedKnown && completed >= 0 && completed == requested {
		return inputDeliveryTargetReport
	}
	return "partial-or-unreported"
}

func receiptProblems(receipt map[string]any, key string) bool {
	switch value := receipt[key].(type) {
	case []any:
		return len(value) > 0
	case []string:
		return len(value) > 0
	default:
		return false
	}
}

func stringField(receipt map[string]any, key string) string {
	value, _ := receipt[key].(string)
	return value
}
