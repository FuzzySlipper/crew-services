package target

import (
	"bytes"
	"encoding/json"
)

// ValidateBatch applies the target's native input contract before a local
// caller admits an operation. The target validates again at delivery.
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
	_, err = validateSteps(raw)
	return err
}
