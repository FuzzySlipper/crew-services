package input

import (
	"encoding/json"
	"testing"
)

func TestValidateBatch(t *testing.T) {
	valid := []string{
		`[{"kind":"hold","keys":[87],"ms":200}]`,
		`[{"kind":"move","dx":40,"dy":-10}]`,
		`[{"kind":"point","x":1279,"y":0,"width":1280,"height":720},{"kind":"click"}]`,
		`[{"kind":"gamepad","lx":0.5,"rt":1,"buttons":["a","lb"],"ms":500}]`,
		`[{"kind":"wait","ms":10000}]`,
		`[{"kind":"wheel","dy":-120}]`,
		`[{"kind":"point","x":10,"y":10,"width":100,"height":100},{"kind":"down","button":3},{"kind":"point","x":60,"y":10,"width":100,"height":100},{"kind":"up","button":3}]`,
	}
	invalid := []string{
		`[]`, `[{"ms":1}]`, `[{"kind":"unknown"}]`,
		`[{"kind":"hold","keys":["W"],"ms":1}]`,
		`[{"kind":"hold","keys":[256]}]`,
		`[{"kind":"move","dx":1}]`,
		`[{"kind":"point","x":0,"y":0,"width":1}]`,
		`[{"kind":"click","button":4}]`,
		`[{"kind":"wait","ms":-1}]`,
		`[{"kind":"wait","ms":6000},{"kind":"wait","ms":4001}]`,
		`[{"kind":"gamepad","lx":2}]`,
		`[{"kind":"gamepad","lt":-0.1}]`,
		`[{"kind":"gamepad","buttons":["z"]}]`,
		`[{"kind":"gamepad","back":true}]`,
		`[{"kind":"wait","extra":1}]`,
		`[{"kind":"wheel","dy":10001}]`,
		`[{"kind":"wheel","dz":1}]`,
		`[{"kind":"down","button":4}]`,
	}
	for _, batch := range valid {
		if err := ValidateBatch(decode(t, batch)); err != nil {
			t.Errorf("%s: %v", batch, err)
		}
	}
	for _, batch := range invalid {
		if err := ValidateBatch(decode(t, batch)); err == nil {
			t.Errorf("%s: accepted", batch)
		}
	}
}

func decode(t *testing.T, text string) []map[string]any {
	t.Helper()
	var steps []map[string]any
	if err := json.Unmarshal([]byte(text), &steps); err != nil {
		t.Fatal(err)
	}
	return steps
}
