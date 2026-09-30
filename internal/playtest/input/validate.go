// Package input owns the backend-neutral playtest input batch contract.
// Backends translate validated steps into their own delivery mechanism.
package input

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// MaxBatchMS bounds the total duration of one batch.
const MaxBatchMS = 10000

// GamepadButtons are the accepted Xbox button names.
var GamepadButtons = map[string]bool{"up": true, "down": true, "left": true, "right": true, "start": true, "back": true, "ls": true, "rs": true, "lb": true, "rb": true, "guide": true, "a": true, "b": true, "x": true, "y": true}

var allowedFields = map[string]map[string]struct{}{
	"hold":    {"kind": {}, "ms": {}, "keys": {}},
	"move":    {"kind": {}, "ms": {}, "dx": {}, "dy": {}},
	"point":   {"kind": {}, "ms": {}, "x": {}, "y": {}, "width": {}, "height": {}},
	"click":   {"kind": {}, "ms": {}, "button": {}},
	"wait":    {"kind": {}, "ms": {}},
	"gamepad": {"kind": {}, "ms": {}, "buttons": {}, "lx": {}, "ly": {}, "rx": {}, "ry": {}, "lt": {}, "rt": {}},
}

// ValidateBatch rejects a malformed batch before any backend delivers it.
// Keys are Windows virtual-key codes, the canonical encoding produced by the
// client's named-key normalization.
func ValidateBatch(steps []map[string]any) error {
	data, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw any
	if err = decoder.Decode(&raw); err != nil {
		return err
	}
	values, ok := raw.([]any)
	if !ok || len(values) < 1 || len(values) > 100 {
		return errors.New("Provide 1..100 steps")
	}
	total := 0
	for _, value := range values {
		step, ok := value.(map[string]any)
		if !ok {
			return errors.New("step must be an object")
		}
		kind, _ := step["kind"].(string)
		allowed, known := allowedFields[kind]
		if !known {
			return errors.New("Unknown input kind")
		}
		for field := range step {
			if _, ok := allowed[field]; ok {
				continue
			}
			if kind == "gamepad" && field == "back" {
				return errors.New("unknown gamepad field back; use buttons:['back']")
			}
			return fmt.Errorf("unknown %s input field %q", kind, field)
		}
		ms, err := integer(fallback(step, "ms", 0), 0, MaxBatchMS, "ms")
		if err != nil {
			return err
		}
		if total += ms; total > MaxBatchMS {
			return fmt.Errorf("A batch may last at most %d ms", MaxBatchMS)
		}
		if err := validateKind(step, kind); err != nil {
			return err
		}
	}
	return nil
}

func validateKind(step map[string]any, kind string) error {
	switch kind {
	case "hold":
		keys, ok := fallback(step, "keys", []any{}).([]any)
		if !ok || len(keys) < 1 || len(keys) > 8 {
			return errors.New("hold needs 1..8 Windows virtual-key codes")
		}
		for _, key := range keys {
			if _, err := integer(key, 1, 255, "key"); err != nil {
				return err
			}
		}
	case "move":
		for _, field := range []string{"dx", "dy"} {
			if _, err := integer(step[field], -32768, 32767, field); err != nil {
				return err
			}
		}
	case "point":
		width, err := integer(step["width"], 1, 16384, "width")
		if err != nil {
			return err
		}
		height, err := integer(step["height"], 1, 16384, "height")
		if err != nil {
			return err
		}
		if _, err = integer(step["x"], 0, width-1, "x"); err != nil {
			return err
		}
		if _, err = integer(step["y"], 0, height-1, "y"); err != nil {
			return err
		}
	case "click":
		if _, err := integer(fallback(step, "button", 1), 1, 3, "button"); err != nil {
			return err
		}
	case "gamepad":
		if raw, exists := step["buttons"]; exists {
			values, ok := raw.([]any)
			if !ok {
				return errors.New("buttons must be an array")
			}
			for _, value := range values {
				if name, ok := value.(string); !ok || !GamepadButtons[name] {
					return errors.New("Unknown Xbox button")
				}
			}
		}
		for _, name := range []string{"lx", "ly", "rx", "ry"} {
			if value, exists := step[name]; exists {
				if _, err := unit(value, -1, name); err != nil {
					return err
				}
			}
		}
		for _, name := range []string{"lt", "rt"} {
			if value, exists := step[name]; exists {
				if _, err := unit(value, 0, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func fallback(step map[string]any, name string, value any) any {
	if v, ok := step[name]; ok {
		return v
	}
	return value
}

func integer(value any, low, high int, name string) (int, error) {
	var v int64
	var err error
	switch n := value.(type) {
	case int: // documented defaults
		v = int64(n)
	case json.Number:
		v, err = n.Int64()
	default:
		err = errors.New("not an integer")
	}
	if err != nil || v < int64(low) || v > int64(high) {
		return 0, fmt.Errorf("%s must be an integer in [%d, %d]", name, low, high)
	}
	return int(v), nil
}

func unit(value any, low float64, name string) (float64, error) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be finite in [%g, 1]", name, low)
	}
	v, err := n.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < low || v > 1 {
		return 0, fmt.Errorf("%s must be finite in [%g, 1]", name, low)
	}
	return v, nil
}
