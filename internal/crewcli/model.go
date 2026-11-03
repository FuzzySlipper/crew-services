package crewcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
)

// modelTail is how much of a transcript's end LastModel reads: every turn
// names its model, so the newest one is near the end.
const modelTail = 4 << 20

// LastModel is the model the newest recognized line of a JSONL transcript
// names, or "" when there is none. extract returns a line's model, or "".
func LastModel(path string, extract func(line []byte) string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(info.Size()-modelTail, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return ""
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if model := extract(lines[i]); model != "" {
			return model
		}
	}
	return ""
}

// About is what a driver reports about a session for the posts it writes:
// the model of its newest turn, and a label naming the agent where its
// runtime has one (a Hermes profile). Both are display information.
type About struct {
	Model string `json:"model"`
	Label string `json:"label,omitempty"`
}

// SessionAbout asks the drivers about the native session self. Any failure
// gives an empty About.
func SessionAbout(ctx context.Context, self string) About {
	if self == "" {
		return About{}
	}
	for _, base := range DriverURLs {
		var out About
		if call(ctx, http.MethodGet, base+"/v1/sessions/"+url.PathEscape(self)+"/model", nil, &out) == nil && (out.Model != "" || out.Label != "") {
			return out
		}
	}
	return About{}
}

// ServeModel answers GET /v1/sessions/{sessionID}/model with only a model.
func ServeModel(model func(r *http.Request, sessionID string) string) http.HandlerFunc {
	return ServeAbout(func(r *http.Request, id string) About { return About{Model: model(r, id)} })
}

// ServeAbout answers GET /v1/sessions/{sessionID}/model from about.
func ServeAbout(about func(r *http.Request, sessionID string) About) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(about(r, r.PathValue("sessionID")))
	}
}
