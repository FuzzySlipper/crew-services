package main

import (
	"encoding/json"
	"testing"

	"crew-services/internal/playtest/assistant"
)

func TestConfigNavigationInitialTarget(t *testing.T) {
	var cfg config
	if err := json.Unmarshal([]byte(`{"session_id":"session-1","navigation":{"initial_target":"exit_1"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Navigation == nil || cfg.Navigation.InitialTarget != "exit_1" {
		t.Fatalf("navigation = %+v", cfg.Navigation)
	}
	if err := cfg.Navigation.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsUnsafeNavigationTarget(t *testing.T) {
	var cfg config
	if err := json.Unmarshal([]byte(`{"navigation":{"initial_target":"not a target"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Navigation.Validate(); err == nil {
		t.Fatal("accepted unsafe navigation target")
	}
}

func TestConfigParentVision(t *testing.T) {
	var cfg config
	if err := json.Unmarshal([]byte(`{"parent_vision":true}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.ParentVision {
		t.Fatal("parent_vision was not decoded")
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted parent_vision without parent_model")
	}
	cfg.ParentModel = "parent"
	cfg.Policy = assistant.Policy{
		Goal:              "reach the exit",
		BudgetMS:          100,
		DecisionTimeoutMS: 100,
		ProgressPointers:  []string{"/facts/game/progress"},
		Tactics: []assistant.Tactic{{
			ID:          "forward",
			Description: "move forward",
			Steps:       []map[string]any{{"kind": "hold", "keys": []int{87}, "ms": 10}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
