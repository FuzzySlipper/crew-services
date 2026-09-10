package session

import (
	"strings"
	"testing"
)

func TestGamescopeReadinessRequiresFocusedMappedGameWindowAtStreamSize(t *testing.T) {
	properties := "WM_CLASS(STRING) = \"Navigator\", \"firefox\"\n_NET_WM_NAME(UTF8_STRING) = \"CraftSurvive — Mozilla Firefox\"\n Width: 1280\n Height: 720\n Map State: IsViewable\n"
	if !gamescopeBrowserReady(properties, "CraftSurvive") {
		t.Fatal("focused game window rejected")
	}
	for name, candidate := range map[string]string{
		"wrong title":  strings.ReplaceAll(properties, "CraftSurvive", "Restore Session"),
		"helper class": strings.ReplaceAll(properties, "firefox", "other"),
		"unmapped":     strings.ReplaceAll(properties, "IsViewable", "IsUnMapped"),
		"wrong size":   strings.ReplaceAll(properties, "1280", "1162"),
	} {
		t.Run(name, func(t *testing.T) {
			if gamescopeBrowserReady(candidate, "CraftSurvive") {
				t.Fatal("unready browser accepted")
			}
		})
	}
}
