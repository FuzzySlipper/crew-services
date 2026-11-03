package hermesdriver

import (
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed assets/deliver.py assets/crew/plugin.yaml assets/crew/__init__.py
var assets embed.FS

// PluginName is the crew plugin's name in each profile's plugins/ directory
// and in its plugins.enabled list.
const PluginName = "crew"

// Install writes the delivery helper into crew-hermes's directory and the
// crew plugin into every profile, and returns the profiles it installed into.
// Enabling the plugin is a separate, explicit step (`hermes plugins enable`).
func (h *Hermes) Install() ([]string, error) {
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return nil, err
	}
	helper, _ := assets.ReadFile("assets/deliver.py")
	if err := os.WriteFile(filepath.Join(h.Dir, "deliver.py"), helper, 0o600); err != nil {
		return nil, err
	}
	plugin, _ := fs.Sub(assets, "assets/crew")
	var installed []string
	for _, profile := range h.Profiles() {
		dir := filepath.Join(h.Home(profile), "plugins", PluginName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return installed, err
		}
		for _, name := range []string{"plugin.yaml", "__init__.py"} {
			data, _ := fs.ReadFile(plugin, name)
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				return installed, err
			}
		}
		installed = append(installed, profile)
	}
	return installed, nil
}

// WriteIntro records what the crew plugin tells each new chat of profile:
// its crew address, seats and commands.
func (h *Hermes) WriteIntro(profile, text string) error {
	dir := filepath.Join(h.Dir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"context": text})
	tmp := filepath.Join(dir, "."+profile+".json")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, profile+".json"))
}
