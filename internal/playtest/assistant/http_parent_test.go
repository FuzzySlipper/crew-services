package assistant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const parentGuidanceJSON = `{"situation":"enemy ahead","objective":"hold range","navigation_target":"exit_1","parameters":{"distance":6},"preferred_tactics":["forward"],"stop":false}`

func writeParentGuidance(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, parentGuidanceJSON)
}

func writeTestPNG(t *testing.T) (string, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 1))
	canvas.Set(0, 0, color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff})
	canvas.Set(1, 0, color.RGBA{R: 0xab, G: 0xcd, B: 0xef, A: 0xff})
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "current.png")
	if err := os.WriteFile(path, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, buffer.Bytes()
}

func TestParentParsesResponsesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		text := parentGuidanceJSON
		event, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
		fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n", event)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Model: "parent", Protocol: "responses"}
	d, e := p.Advise(context.Background(), State{}, []Tactic{{ID: "forward"}})
	if e != nil || d.Guidance.Objective != "hold range" || d.Guidance.NavigationTarget != "exit_1" {
		t.Fatalf("%+v %v", d, e)
	}
}

func TestParentVisionAttachesCapturePNGToChat(t *testing.T) {
	path, pixels := writeTestPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatal(r.URL.Path)
		}
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 2 || !strings.Contains(string(request.Messages[0].Content), "current PNG") {
			t.Fatalf("messages = %#v", request.Messages)
		}
		var content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(request.Messages[1].Content, &content); err != nil {
			t.Fatal(err)
		}
		if len(content) != 2 || content[0].Type != "text" || content[1].Type != "image_url" {
			t.Fatalf("content = %+v", content)
		}
		encoded := strings.TrimPrefix(content[1].ImageURL.URL, "data:image/png;base64,")
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !bytes.Equal(got, pixels) {
			t.Fatalf("attached image = %x, %v", got, err)
		}
		writeParentGuidance(w)
	}))
	defer server.Close()
	observed := time.Now().UTC().Truncate(time.Millisecond)
	p := HTTPParent{BaseURL: server.URL, Model: "parent", Vision: true}
	d, err := p.Advise(context.Background(), State{Observation: Observation{CapturedAt: observed, Capture: json.RawMessage(`{"path":` + strconvQuote(path) + `,"artifact_id":"artifact-7"}`)}}, []Tactic{{ID: "forward"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pixels)
	if d.InputImage == nil || d.InputImage.Path != path || d.InputImage.SHA256 != fmt.Sprintf("%x", sum) || d.InputImage.Bytes != len(pixels) || !d.InputImage.ObservedAt.Equal(observed) || d.InputImage.CaptureID != "artifact-7" {
		t.Fatalf("image evidence = %+v", d.InputImage)
	}
}

func TestParentVisionAttachesCapturePNGToResponses(t *testing.T) {
	path, pixels := writeTestPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		var request struct {
			Input []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL string `json:"image_url"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Input) != 1 || len(request.Input[0].Content) != 2 || request.Input[0].Content[0].Type != "input_text" || request.Input[0].Content[1].Type != "input_image" {
			t.Fatalf("input = %+v", request.Input)
		}
		got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(request.Input[0].Content[1].ImageURL, "data:image/png;base64,"))
		if err != nil || !bytes.Equal(got, pixels) {
			t.Fatalf("attached image = %x, %v", got, err)
		}
		writeParentGuidance(w)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Model: "parent", Protocol: "responses", Vision: true}
	if _, err := p.Advise(context.Background(), State{Observation: Observation{Capture: json.RawMessage(`{"path":` + strconvQuote(path) + `,"capture_id":"capture-9"}`)}}, []Tactic{{ID: "forward"}}); err != nil {
		t.Fatal(err)
	}
}

func TestParentTextOnlyDoesNotReadCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 2 || string(request.Messages[1].Content) == "" || request.Messages[1].Content[0] != '"' || bytes.Contains(request.Messages[1].Content, []byte("image_url")) {
			t.Fatalf("text-only content = %s", request.Messages[1].Content)
		}
		writeParentGuidance(w)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Model: "parent"}
	_, err := p.Advise(context.Background(), State{Observation: Observation{Capture: json.RawMessage(`{"path":"/not/read/without-vision.png"}`)}}, []Tactic{{ID: "forward"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParentVisionRejectsMissingOrUnreadableCapture(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Model: "parent", Vision: true}
	if _, err := p.Advise(context.Background(), State{}, []Tactic{{ID: "forward"}}); err == nil || !strings.Contains(err.Error(), "current capture") {
		t.Fatalf("missing capture error = %v", err)
	}
	missingPath := filepath.Join(t.TempDir(), "gone.png")
	if _, err := p.Advise(context.Background(), State{Observation: Observation{Capture: json.RawMessage(`{"path":` + strconvQuote(missingPath) + `}`)}}, []Tactic{{ID: "forward"}}); err == nil || !strings.Contains(err.Error(), "read parent vision capture") {
		t.Fatalf("unreadable capture error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("made %d parent requests", requests)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
func TestIncompleteParentStreamCannotInstallGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"{}"}`)
	}))
	defer server.Close()
	p := HTTPParent{BaseURL: server.URL, Protocol: "responses"}
	if _, e := p.Advise(context.Background(), State{}, nil); e == nil {
		t.Fatal("accepted incomplete response")
	}
}
