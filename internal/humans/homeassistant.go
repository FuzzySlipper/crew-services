package humans

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// HomeAssistant pushes through Home Assistant's notify service, which sends
// to the person's phone through the Companion app. crew-humans only talks to
// Home Assistant on the LAN; Home Assistant sends the push as it already does.
type HomeAssistant struct {
	Client *http.Client
}

// Push text is shorter than the inbox's: a phone shows a line or two.
const pushText = 140

func (h HomeAssistant) Push(ctx context.Context, _ Person, route RouteConfig, n Notice) error {
	token, err := os.ReadFile(route.TokenFile)
	if err != nil {
		return Refused{fmt.Errorf("read the Home Assistant token: %w", err)}
	}
	data := map[string]any{
		"channel":    "Crew",
		"importance": "default",
	}
	if n.Urgent {
		data["channel"], data["importance"], data["priority"] = "Crew urgent", "high", "high"
	}
	if n.Link != "" {
		// clickAction for Android, url for iOS.
		data["clickAction"], data["url"] = n.Link, n.Link
	}
	if n.Group != "" {
		data["tag"] = "crew-" + n.Group
	}
	title := n.Title
	if n.Urgent {
		title = "[urgent] " + title
	}
	message := cut(n.Text, pushText)
	if message == "" {
		message = title
	}
	body, _ := json.Marshal(map[string]any{"title": title, "message": message, "data": data})
	endpoint := strings.TrimRight(route.URL, "/") + "/api/services/notify/" + route.Service
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Refused{err}
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	switch {
	case response.StatusCode < 300:
		return nil
	case response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests:
		return Refused{fmt.Errorf("Home Assistant answered %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))}
	}
	return fmt.Errorf("Home Assistant answered %d", response.StatusCode)
}
