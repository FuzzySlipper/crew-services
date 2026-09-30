package devserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func checkHealth(ctx context.Context, client *http.Client, manifest *ServeManifest, port int) HealthResult {
	url := healthURL(manifest.ProbeHost, port, manifest.HealthPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return HealthResult{URL: url, Error: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return HealthResult{URL: url, Error: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	readyTextFound := true
	if strings.TrimSpace(manifest.ReadyText) != "" {
		readyTextFound = strings.Contains(string(body), manifest.ReadyText)
	}
	headerMatched := true
	if strings.TrimSpace(manifest.IdentityHeader) != "" {
		headerMatched = resp.Header.Get(manifest.IdentityHeader) == manifest.ExpectedIdentityHeaderValue()
	}
	matched := resp.StatusCode >= 200 && resp.StatusCode < 300 && readyTextFound && headerMatched
	return HealthResult{
		URL:            url,
		StatusCode:     resp.StatusCode,
		ReadyTextFound: readyTextFound,
		HeaderMatched:  headerMatched,
		Matched:        matched,
	}
}

func waitForHealth(ctx context.Context, client *http.Client, manifest *ServeManifest, port int) (HealthResult, error) {
	deadline := time.Now().Add(manifest.StartupTimeout)
	var last HealthResult
	for {
		last = checkHealth(ctx, client, manifest, port)
		if last.Matched {
			return last, nil
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("dev server did not become healthy at %s", last.URL)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(manifest.HealthInterval):
		}
	}
}
