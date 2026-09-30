package devserver

import (
	"strconv"
	"strings"
)

type templateContext struct {
	project    string
	repoRoot   string
	bindHost   string
	probeHost  string
	port       int
	localURL   string
	publicURL  string
	sessionDir string
}

func renderTemplate(input string, values templateContext) string {
	replacements := map[string]string{
		"{project}":     values.project,
		"{repo_root}":   values.repoRoot,
		"{host}":        values.bindHost,
		"{bind_host}":   values.bindHost,
		"{probe_host}":  values.probeHost,
		"{port}":        strconv.Itoa(values.port),
		"{base_url}":    values.localURL,
		"{local_url}":   values.localURL,
		"{public_url}":  values.publicURL,
		"{session_dir}": values.sessionDir,
	}
	output := input
	for key, value := range replacements {
		output = strings.ReplaceAll(output, key, value)
	}
	return output
}
