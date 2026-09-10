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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// WolfLauncher owns launch plumbing, not game state. Visual readiness is returned
// as an explicit agent check rather than inferred from HTTP or stream availability.
type WolfLauncher struct {
	Backend       Backend
	SSHHost       string
	ForwardBinary string
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func (l *WolfLauncher) docker(ctx context.Context, input io.Reader, args ...string) (string, error) {
	argv := []string{"sudo", "-n", "runuser", "-u", "docker-rt", "--", "env", "DOCKER_HOST=unix:///data/services/docker-rt/run/docker.sock", "docker"}
	argv = append(argv, args...)
	quoted := make([]string, len(argv))
	for i, v := range argv {
		quoted[i] = shellQuote(v)
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", l.SSHHost, strings.Join(quoted, " "))
	cmd.Stdin = input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("remote browser operation: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func (l *WolfLauncher) Launch(ctx context.Context, id string, p Profile) (map[string]any, error) {
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return nil, errors.New("profile URL must be an HTTP game endpoint")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	if _, err = strconv.Atoi(port); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("game server unavailable: %w", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("game server returned HTTP %d", response.StatusCode)
	}
	// Wolf assigns the seed keyboard before starting its UI runner. Joining
	// earlier lets that asynchronous assignment overwrite the lobby keyboard.
	status, err := l.Backend.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	lease, _ := status["lease"].(map[string]any)
	clientID, _ := lease["client_id"].(string)
	if clientID == "" {
		return nil, errors.New("target status omitted the owned client ID")
	}
	seedReady := false
	for range 30 {
		name, e := l.docker(ctx, nil, "ps", "--filter", "name=Wolf-UI_"+clientID, "--format", "{{.Names}}")
		if e != nil {
			return nil, e
		}
		if name == "Wolf-UI_"+clientID {
			seedReady = true
			break
		}
		if err = wait(ctx, 250*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if !seedReady {
		return nil, errors.New("Wolf UI seed runner did not become ready; use application Wolf UI")
	}
	// The controller must exist before browser enumeration and lobby transfer.
	if _, err = l.Backend.Input(ctx, id, []map[string]any{{"kind": "gamepad", "ms": 100}, {"kind": "hold", "keys": []int{27}, "ms": 100}}); err != nil {
		return nil, err
	}
	launchBackend, ok := l.Backend.(interface {
		LaunchFirefox(context.Context, string, string) (map[string]any, error)
	})
	if !ok {
		return nil, errors.New("backend does not support owned Firefox launch")
	}
	launch, err := launchBackend.LaunchFirefox(ctx, id, p.URL)
	if err != nil {
		return nil, err
	}
	lobbyID, _ := launch["lobby_id"].(string)
	if lobbyID == "" {
		return nil, errors.New("target launch returned no owned lobby_id")
	}
	compositor, _ := launch["compositor"].(string)
	startup, _ := launch["browser_startup"].(string)
	runnerName, _ := launch["runner_container_name"].(string)
	if runnerName == "" {
		return nil, errors.New("target launch omitted runner container name")
	}
	var container string
	for range 30 {
		current, e := l.docker(ctx, nil, "ps", "--filter", "name="+runnerName+"_"+lobbyID, "--format", "{{.ID}}")
		if e != nil {
			return nil, e
		}
		found := strings.Fields(current)
		if len(found) > 1 {
			return nil, errors.New("multiple new Firefox containers; cannot identify owned browser")
		}
		if len(found) == 1 {
			container = found[0]
			break
		}
		if err = wait(ctx, time.Second); err != nil {
			return nil, err
		}
	}
	if container == "" {
		return nil, errors.New("owned Firefox lobby did not start its container")
	}
	localURL := *u
	localURL.Host = "localhost:" + port
	if startup != "kiosk" {
		f, err := os.Open(l.ForwardBinary)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err = l.docker(ctx, f, "exec", "-i", container, "sh", "-c", "cat > /tmp/playtest-forward && chmod 755 /tmp/playtest-forward"); err != nil {
			return nil, err
		}
		if _, err = l.docker(ctx, nil, "exec", "-d", container, "/tmp/playtest-forward", "--port", port, "--host", u.Hostname(), "--target-port", port); err != nil {
			return nil, err
		}
		steps := []map[string]any{{"kind": "hold", "keys": []int{17, 76}, "ms": 100}, {"kind": "wait", "ms": 200}}
		for _, r := range localURL.String() {
			key, shift, e := urlKey(r)
			if e != nil {
				return nil, e
			}
			keys := []int{key}
			if shift {
				keys = append([]int{16}, keys...)
			}
			steps = append(steps, map[string]any{"kind": "hold", "keys": keys, "ms": 25})
		}
		steps = append(steps, map[string]any{"kind": "hold", "keys": []int{13}, "ms": 100})
		if len(steps) > 100 {
			return nil, errors.New("profile URL too long for native navigation batch")
		}
		if err = l.waitBrowser(ctx, container, "", compositor); err != nil {
			return nil, err
		}
		// Firefox maps its window before its address-bar event handlers are ready.
		if err = wait(ctx, time.Second); err != nil {
			return nil, err
		}
		if _, err = l.Backend.Input(ctx, id, steps); err != nil {
			return nil, err
		}
	}
	if err = l.waitBrowser(ctx, container, p.WindowTitle, compositor); err != nil {
		return nil, err
	}
	if err = wait(ctx, time.Second); err != nil {
		return nil, err
	}
	focus := []map[string]any{{"kind": "point", "x": 640, "y": 460, "width": 1280, "height": 720}, {"kind": "wait", "ms": 200}, {"kind": "click", "button": 1, "ms": 100}}
	if startup != "kiosk" {
		focus = append(focus, map[string]any{"kind": "hold", "keys": []int{9}, "ms": 100})
	}
	if _, err = l.Backend.Input(ctx, id, focus); err != nil {
		return nil, err
	}
	if err = wait(ctx, time.Second); err != nil {
		return nil, err
	}
	observation, err := l.Backend.Observe(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": p.URL, "browser_url": localURL.String(), "container": container, "server_http_ready": true, "native_focus_sequence_sent": true, "game_readiness": "requires visual confirmation of this observation", "observation": observation, "controls": p.Controls, "compositor": compositor, "browser_startup": startup}, nil
}

// Wait for the actual browser window, rather than equating a running container
// with a keyboard recipient. Queries are compositor reads, not game injection.
func (l *WolfLauncher) waitBrowser(ctx context.Context, container, title, compositor string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		if compositor == "gamescope" {
			properties, err := l.docker(ctx, nil, "exec", "--user", "retro", container, "env", "DISPLAY=:0", "LC_ALL=C.UTF-8", "sh", "-c", gamescopeWindowProbe)
			if err == nil && gamescopeBrowserReady(properties, title) {
				return nil
			}
			if err = wait(ctx, 250*time.Millisecond); err != nil {
				return fmt.Errorf("Gamescope browser window not ready for %q: %w", title, err)
			}
			continue
		}
		tree, err := l.docker(ctx, nil, "exec", container, "sh", "-c", `for socket in "$XDG_RUNTIME_DIR/sway.socket" "$XDG_RUNTIME_DIR"/sway-ipc*.sock /run/user/*/sway-ipc*.sock /tmp/sway-ipc*.sock; do if [ -S "$socket" ]; then exec swaymsg -s "$socket" -t get_tree; fi; done; exit 1`)
		if err == nil {
			var node any
			if json.Unmarshal([]byte(tree), &node) == nil && browserInTree(node, title) {
				return nil
			}
		}
		if err = wait(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("browser window not ready for %q: %w", title, err)
		}
	}
}

func browserInTree(node any, title string) bool {
	switch value := node.(type) {
	case map[string]any:
		app, _ := value["app_id"].(string)
		if app == "" {
			if properties, ok := value["window_properties"].(map[string]any); ok {
				app, _ = properties["class"].(string)
			}
		}
		name, _ := value["name"].(string)
		if strings.Contains(strings.ToLower(app), "firefox") && strings.Contains(strings.ToLower(name), strings.ToLower(title)) {
			return true
		}
		for _, child := range value {
			if browserInTree(child, title) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if browserInTree(child, title) {
				return true
			}
		}
	}
	return false
}

func urlKey(r rune) (int, bool, error) {
	if r >= 'a' && r <= 'z' {
		return int(r - 'a' + 'A'), false, nil
	}
	if r >= 'A' && r <= 'Z' {
		return int(r), true, nil
	}
	if r >= '0' && r <= '9' {
		return int(r), false, nil
	}
	switch r {
	case ':':
		return 186, true, nil
	case '/':
		return 191, false, nil
	case '.':
		return 190, false, nil
	case '-':
		return 189, false, nil
	case '_':
		return 189, true, nil
	case '?':
		return 191, true, nil
	case '=':
		return 187, false, nil
	case '&':
		return 55, true, nil
	case '%':
		return 53, true, nil
	case '#':
		return 51, true, nil
	}
	return 0, false, fmt.Errorf("unsupported URL character %q", r)
}

// Inspect the window Gamescope actually selected, not an unrelated Firefox
// helper window. These X11 queries do not inject input or inspect game state.
const gamescopeWindowProbe = `window=$(xprop -root GAMESCOPE_FOCUSED_WINDOW | sed -n 's/^GAMESCOPE_FOCUSED_WINDOW(CARDINAL) = \([0-9][0-9]*\)$/\1/p')
[ -n "$window" ] && [ "$window" != 0 ] || exit 1
xprop -id "$window" WM_CLASS _NET_WM_NAME
xwininfo -id "$window"`

func gamescopeBrowserReady(properties, title string) bool {
	class, name := "", ""
	width, height, mapped := false, false, false
	for _, line := range strings.Split(properties, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "WM_CLASS(STRING) = "):
			class = strings.ToLower(line)
		case strings.HasPrefix(line, "_NET_WM_NAME(UTF8_STRING) = "):
			name = strings.TrimPrefix(line, "_NET_WM_NAME(UTF8_STRING) = ")
		case line == "Width: 1280":
			width = true
		case line == "Height: 720":
			height = true
		case line == "Map State: IsViewable":
			mapped = true
		}
	}
	return strings.Contains(class, "\"firefox\"") && strings.Contains(strings.ToLower(name), strings.ToLower(title)) && width && height && mapped
}
