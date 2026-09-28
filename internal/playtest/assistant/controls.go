package assistant

import (
	"errors"
	"fmt"
	"strings"
)

// ControlGroup owns disjoint fields in one standard gamepad state. All options
// retain caller-authored values and the same finite hold duration.
type ControlGroup struct {
	ID           string   `json:"id"`
	Instructions string   `json:"instructions"`
	Tactics      []string `json:"tactics"`
}

func validateGroups(groups []ControlGroup, tactics []Tactic) error {
	if len(groups) == 0 {
		return nil
	}
	if len(groups) > 4 {
		return errors.New("at most four control groups")
	}
	byID := map[string]Tactic{}
	for _, t := range tactics {
		byID[t.ID] = t
	}
	owners := map[string]string{}
	ids := map[string]bool{}
	duration := ""
	for _, g := range groups {
		if g.ID == "" || g.ID == "status" || ids[g.ID] || len(g.Tactics) == 0 {
			return errors.New("control groups require unique non-status IDs and options")
		}
		ids[g.ID] = true
		seen := map[string]bool{}
		for _, id := range g.Tactics {
			t, ok := byID[id]
			if !ok || seen[id] || len(t.Steps) != 1 {
				return fmt.Errorf("group %s needs unique single-step tactics: %s", g.ID, id)
			}
			seen[id] = true
			s := t.Steps[0]
			if s["kind"] != "gamepad" {
				return errors.New("control groups require gamepad steps")
			}
			// JSON equality accommodates Go fixture ints and decoded JSON numbers.
			d := fmt.Sprint(s["ms"])
			if duration == "" {
				duration = d
			}
			if duration != d {
				return errors.New("grouped controls require a common hold duration")
			}
			for k := range s {
				if k == "kind" || k == "ms" {
					continue
				}
				if owner, ok := owners[k]; ok && owner != g.ID {
					return fmt.Errorf("control field %s overlaps groups %s and %s", k, owner, g.ID)
				}
				owners[k] = g.ID
			}
		}
	}
	return nil
}

func composeControls(groups []ControlGroup, tactics []Tactic, choices map[string]string) (*Tactic, error) {
	if len(choices) != len(groups) {
		return nil, errors.New("missing or extra control choices")
	}
	byID := map[string]Tactic{}
	for _, t := range tactics {
		byID[t.ID] = t
	}
	state := map[string]any{"kind": "gamepad"}
	parts := []string{}
	for _, g := range groups {
		id := choices[g.ID]
		allowed := false
		for _, x := range g.Tactics {
			if x == id {
				allowed = true
			}
		}
		if !allowed {
			return nil, fmt.Errorf("invalid %s choice %s", g.ID, id)
		}
		t := byID[id]
		for k, v := range t.Steps[0] {
			state[k] = v
		}
		parts = append(parts, g.ID+"="+id)
	}
	return &Tactic{ID: strings.Join(parts, ";"), Description: "composed fixed gamepad controls", Steps: []map[string]any{state}}, nil
}
