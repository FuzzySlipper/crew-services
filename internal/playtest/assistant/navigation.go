package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var navigationTargetID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NavigationConfig opts an interval into product-provided route observations.
// The target is a product ID, never free-form route text.
type NavigationConfig struct {
	InitialTarget string `json:"initial_target"`
}

func (c *NavigationConfig) Validate() error {
	if c == nil {
		return nil
	}
	if !validNavigationTarget(c.InitialTarget) {
		return errors.New("navigation.initial_target must use 1..64 ASCII letters, digits, _ or -")
	}
	return nil
}

func validNavigationTarget(id string) bool { return navigationTargetID.MatchString(id) }

func navigationTargetIDs(raw json.RawMessage) (map[string]struct{}, error) {
	var response struct {
		Targets []struct {
			ID string `json:"id"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid navigation.targets result: %w", err)
	}
	if response.Targets == nil {
		return nil, errors.New("invalid navigation.targets result: targets is required")
	}
	ids := make(map[string]struct{}, len(response.Targets))
	for _, target := range response.Targets {
		if !validNavigationTarget(target.ID) {
			return nil, errors.New("invalid navigation.targets result: target ID is invalid")
		}
		if _, exists := ids[target.ID]; exists {
			return nil, errors.New("invalid navigation.targets result: duplicate target ID")
		}
		ids[target.ID] = struct{}{}
	}
	return ids, nil
}

func observationNavigationTargets(observation Observation) (map[string]struct{}, bool, error) {
	raw, ok := observation.Facts["navigation.targets"]
	if !ok {
		return nil, false, nil
	}
	ids, err := navigationTargetIDs(raw)
	return ids, true, err
}
