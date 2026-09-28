package assistant

import (
	"errors"
	"math"
	"strings"
	"time"
)

const (
	maxDecisionViewFields    = 32
	maxDecisionViewDeltas    = 32
	maxDecisionViewPositions = 16
	maxDecisionViewPointer   = 256
	unknownDecisionFact      = "unknown"
)

// DecisionView selects the small set of product observations useful to a
// controller decision. Names are supplied by the product configuration and
// values are JSON pointers into an Observation document.
type DecisionView struct {
	Fields    map[string]string `json:"fields"`
	Deltas    map[string]string `json:"deltas,omitempty"`
	Positions map[string]string `json:"positions,omitempty"`
}

// Validate keeps the model context finite while allowing products to name the
// facts that matter to their decisions.
func (v *DecisionView) Validate() error {
	if v == nil {
		return nil
	}
	if err := validateDecisionPointers("fields", v.Fields, maxDecisionViewFields); err != nil {
		return err
	}
	if err := validateDecisionPointers("deltas", v.Deltas, maxDecisionViewDeltas); err != nil {
		return err
	}
	return validateDecisionPointers("positions", v.Positions, maxDecisionViewPositions)
}

func validateDecisionPointers(kind string, values map[string]string, limit int) error {
	if len(values) > limit {
		return errors.New(kind + " has too many entries")
	}
	for name, path := range values {
		if strings.TrimSpace(name) == "" || len(name) > 80 {
			return errors.New(kind + " requires nonempty names up to 80 characters")
		}
		if !validDecisionPointer(path) {
			return errors.New(kind + " requires JSON pointers up to 256 characters")
		}
	}
	return nil
}

func validDecisionPointer(path string) bool {
	if len(path) < 2 || len(path) > maxDecisionViewPointer || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "/capture") {
		return false
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" {
			return false
		}
	}
	return true
}

// State returns a compact, JSON-friendly decision snapshot. It intentionally
// has no capture, unselected fact map, or raw prior observation.
func (v *DecisionView) State(s State) any {
	if v == nil {
		return semanticState(s)
	}

	current := observationDocument(s.Observation)
	var previous any
	if s.Previous != nil {
		previous = observationDocument(*s.Previous)
	}
	observation := map[string]any{
		"captured_at": decisionTimestamp(s.Observation.CapturedAt),
		"fields":      decisionFields(current, v.Fields),
	}
	if len(v.Deltas) > 0 {
		observation["deltas"] = decisionDeltas(current, previous, v.Deltas)
	}
	if len(v.Positions) > 0 {
		observation["positions"] = decisionPositions(current, previous, v.Positions)
	}
	if s.Previous == nil || s.Previous.CapturedAt.IsZero() {
		observation["previous_observation_age_ms"] = unknownDecisionFact
		observation["sample_interval_ms"] = unknownDecisionFact
	} else {
		age := time.Since(s.Previous.CapturedAt).Milliseconds()
		if age < 0 {
			observation["previous_observation_age_ms"] = unknownDecisionFact
		} else {
			observation["previous_observation_age_ms"] = age
		}
		if s.Observation.CapturedAt.IsZero() {
			observation["sample_interval_ms"] = unknownDecisionFact
		} else {
			interval := s.Observation.CapturedAt.Sub(s.Previous.CapturedAt).Milliseconds()
			if interval < 0 {
				observation["sample_interval_ms"] = unknownDecisionFact
			} else {
				observation["sample_interval_ms"] = interval
			}
		}
	}

	state := map[string]any{
		"goal":           s.Goal,
		"instructions":   s.Instructions,
		"remaining_ms":   s.RemainingMS,
		"last_action":    s.LastAction,
		"recent_actions": append([]string(nil), s.RecentActions...),
		"actions_taken":  s.ActionsTaken,
		"observation":    observation,
	}
	if s.Guidance != nil {
		guidance := *s.Guidance
		state["parent_guidance"] = guidance
	}
	return state
}

func decisionTimestamp(t time.Time) string {
	if t.IsZero() {
		return unknownDecisionFact
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func decisionFields(document any, selections map[string]string) map[string]any {
	result := make(map[string]any, len(selections))
	for name, path := range selections {
		value, ok := pointer(document, path)
		copied, valid := decisionValue(value)
		if !ok || !valid {
			result[name] = unknownDecisionFact
			continue
		}
		result[name] = copied
	}
	return result
}

func decisionDeltas(current, previous any, selections map[string]string) map[string]any {
	result := make(map[string]any, len(selections))
	for name, path := range selections {
		currentValue, currentOK := pointer(current, path)
		previousValue, previousOK := pointer(previous, path)
		currentNumber, currentFinite := decisionNumber(currentValue)
		previousNumber, previousFinite := decisionNumber(previousValue)
		if !currentOK || !previousOK || !currentFinite || !previousFinite {
			result[name] = unknownDecisionFact
			continue
		}
		delta := currentNumber - previousNumber
		if !finiteDecisionNumber(delta) {
			result[name] = unknownDecisionFact
			continue
		}
		result[name] = delta
	}
	return result
}

func decisionPositions(current, previous any, selections map[string]string) map[string]any {
	result := make(map[string]any, len(selections))
	for name, path := range selections {
		currentPosition, currentOK := decisionPosition(current, path)
		if !currentOK {
			result[name] = unknownDecisionFact
			continue
		}
		view := map[string]any{"x": currentPosition[0], "y": currentPosition[1], "z": currentPosition[2]}
		previousPosition, previousOK := decisionPosition(previous, path)
		if !previousOK {
			view["dx"] = unknownDecisionFact
			view["dy"] = unknownDecisionFact
			view["dz"] = unknownDecisionFact
			view["displacement"] = unknownDecisionFact
		} else {
			dx := currentPosition[0] - previousPosition[0]
			dy := currentPosition[1] - previousPosition[1]
			dz := currentPosition[2] - previousPosition[2]
			displacement := math.Sqrt(dx*dx + dy*dy + dz*dz)
			if !finiteDecisionNumber(dx) || !finiteDecisionNumber(dy) || !finiteDecisionNumber(dz) || !finiteDecisionNumber(displacement) {
				view["dx"] = unknownDecisionFact
				view["dy"] = unknownDecisionFact
				view["dz"] = unknownDecisionFact
				view["displacement"] = unknownDecisionFact
				result[name] = view
				continue
			}
			view["dx"] = dx
			view["dy"] = dy
			view["dz"] = dz
			view["displacement"] = displacement
		}
		result[name] = view
	}
	return result
}

func decisionPosition(document any, path string) ([3]float64, bool) {
	value, ok := pointer(document, path)
	object, ok := value.(map[string]any)
	if !ok {
		return [3]float64{}, false
	}
	x, xOK := decisionNumber(object["x"])
	y, yOK := decisionNumber(object["y"])
	z, zOK := decisionNumber(object["z"])
	return [3]float64{x, y, z}, xOK && yOK && zOK
}

func decisionValue(value any) (any, bool) {
	switch value := value.(type) {
	case nil, string, bool:
		return value, true
	case float64:
		if finiteDecisionNumber(value) {
			return value, true
		}
	case []any:
		copied := make([]any, len(value))
		for index, item := range value {
			var ok bool
			copied[index], ok = decisionValue(item)
			if !ok {
				return nil, false
			}
		}
		return copied, true
	case map[string]any:
		copied := make(map[string]any, len(value))
		for key, item := range value {
			var ok bool
			copied[key], ok = decisionValue(item)
			if !ok {
				return nil, false
			}
		}
		return copied, true
	}
	return nil, false
}

func decisionNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok && finiteDecisionNumber(number)
}

func finiteDecisionNumber(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
