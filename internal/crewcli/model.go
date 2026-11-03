package crewcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
)

// modelChunk is how much of a transcript LastModel reads at a time, walking
// back from the end until it finds a recognized line.
const modelChunk = 1 << 20

// LastModel is the model the newest recognized line of a JSONL transcript
// names, or "" when there is none. extract returns a line's model, or "". It
// reads the file backward in bounded chunks, so a model named before a long
// turn's output (a Codex turn_context ahead of megabytes of tool results) is
// still found.
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
	// carry is the start of a line whose beginning is in an earlier chunk.
	var carry []byte
	for end := info.Size(); end > 0; {
		start := max(end-modelChunk, 0)
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return ""
		}
		data := append(chunk, carry...)
		lines := bytes.Split(data, []byte("\n"))
		// The first piece may be the end of a line that began before start;
		// it is finished with the next chunk, unless this is the file's start.
		first := 0
		if start > 0 {
			first = 1
			carry = append([]byte(nil), lines[0]...)
		}
		for i := len(lines) - 1; i >= first; i-- {
			if model := extract(lines[i]); model != "" {
				return model
			}
		}
		end = start
	}
	return ""
}

// About is what a driver reports about a session for the posts it writes:
// the model of its newest turn, a label naming the agent where its runtime
// has one (a Hermes profile), and the person the session works for. All are
// display information.
type About struct {
	Model  string `json:"model"`
	Label  string `json:"label,omitempty"`
	Person string `json:"person,omitempty"`
}

// SessionAbout asks the drivers about the native session self. Any failure
// gives an empty About.
func SessionAbout(ctx context.Context, self string) About {
	if self == "" {
		return About{}
	}
	for _, base := range DriverURLs {
		var out About
		if call(ctx, http.MethodGet, base+"/v1/sessions/"+url.PathEscape(self)+"/model", nil, &out) == nil && (out.Model != "" || out.Label != "" || out.Person != "") {
			return out
		}
	}
	return About{}
}

// ServeModel answers GET /v1/sessions/{sessionID}/model with only a model.
func ServeModel(model func(r *http.Request, sessionID string) string) http.HandlerFunc {
	return ServeAbout(func(r *http.Request, id string) About { return About{Model: model(r, id)} })
}

// ServeAbout answers GET /v1/sessions/{sessionID}/model from about, with the
// session's person resolved as the CLI resolves it (PersonFor) when about
// does not say.
func ServeAbout(about func(r *http.Request, sessionID string) About) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("sessionID")
		out := about(r, id)
		if out.Person == "" {
			out.Person = PersonFor(id)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
