package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// debugQuery is internal transport for fixed, read-only commands selected by
// owning service operations. It is not an arbitrary agent debug executor.
func (s *Service) debugQuery(ctx context.Context, id, gameURL, command, kind, source string) (any, error) {
	origin, err := url.Parse(gameURL)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, errors.New("debug query profile requires an HTTP game origin")
	}
	origin.Path = ""
	origin.RawPath = ""
	origin.RawQuery = ""
	origin.Fragment = ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path, body string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, method, origin.String()+path, strings.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil {
			return nil, resp.StatusCode, err
		}
		if len(raw) > 1<<20 {
			return nil, resp.StatusCode, errors.New("debug response exceeds 1 MiB")
		}
		return raw, resp.StatusCode, nil
	}
	raw, status, err := request("GET", "/__rusty/product/runtime/debug/catalog", "")
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, errors.New("capability_unavailable: product live-debug catalog")
	}
	if status != 200 {
		return nil, fmt.Errorf("debug catalog HTTP %d", status)
	}
	var catalog struct {
		Available bool `json:"available"`
		Commands  []struct {
			Name       string `json:"name"`
			Parameters []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"parameters"`
		} `json:"commands"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("invalid debug catalog: %w", err)
	}
	found := false
	for _, c := range catalog.Commands {
		if c.Name == strings.Fields(command)[0] {
			found = true
		}
	}
	if !catalog.Available || !found {
		return nil, errors.New("capability_unavailable: requested product debug query")
	}
	queryID := newID()
	receipt := map[string]any{"query_id": queryID, "session_id": id, "command": command, "origin": origin.String(), "requested_at": time.Now().UTC(), "source": source, "frame_correlation": "unavailable"}
	path := filepath.Join(s.stateDir, kind+"-"+queryID+".json")
	receipt["evidence_path"] = path
	if err = atomicJSON(path, receipt); err != nil {
		return nil, fmt.Errorf("persist query request: %w", err)
	}
	raw, status, err = request("POST", "/__rusty/product/runtime/debug/execute", command)
	receipt["completed_at"] = time.Now().UTC()
	receipt["http_status"] = status
	receipt["raw_result"] = string(raw)
	if err == nil && status != 200 {
		err = fmt.Errorf("product debug query HTTP %d", status)
	}
	if err == nil {
		var facts map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decodeErr := decoder.Decode(&facts); decodeErr != nil || facts == nil {
			err = errors.New("product debug query did not return a JSON object")
		} else if decoder.Decode(new(any)) != io.EOF {
			err = errors.New("product debug query returned trailing data")
		} else {
			receipt["facts"] = facts
		}
	}
	if err != nil {
		receipt["error"] = err.Error()
	}
	if persistErr := atomicJSON(path, receipt); persistErr != nil {
		return receipt, fmt.Errorf("persist interaction result: %w", persistErr)
	}
	return receipt, err
}
