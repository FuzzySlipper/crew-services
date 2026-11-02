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

// SessionModel asks the drivers which model the native session self is
// using, for the posts it writes. It is display information, so any failure
// gives "".
func SessionModel(ctx context.Context, self string) string {
	if self == "" {
		return ""
	}
	for _, base := range DriverURLs {
		var out struct {
			Model string `json:"model"`
		}
		if call(ctx, http.MethodGet, base+"/v1/sessions/"+url.PathEscape(self)+"/model", nil, &out) == nil && out.Model != "" {
			return out.Model
		}
	}
	return ""
}

// ServeModel answers GET /v1/sessions/{sessionID}/model from model.
func ServeModel(model func(r *http.Request, sessionID string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"model": model(r, r.PathValue("sessionID"))})
	}
}
