// Package crewcli holds the runtime-neutral pieces of the drivers' send and
// who commands: the wire shapes each driver serves on its loopback API, the
// merged session list, and recipient lookup.
package crewcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// SessionInfo is one session a driver knows, as served by GET /v1/sessions.
type SessionInfo struct {
	Address  string    `json:"address"`
	Runtime  string    `json:"runtime"`
	NativeID string    `json:"native_id"`
	Cwd      string    `json:"cwd,omitempty"`
	State    string    `json:"state"`
	SeenAt   time.Time `json:"seen_at"`
}

// SendRequest is the body of POST /v1/send. From is the caller's own native
// session id; the driver sends as that session's crew address.
type SendRequest struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Body        string `json:"body"`
	Policy      string `json:"policy,omitempty"`
	OperationID string `json:"operation_id"`
}

type SendResult struct {
	MessageID   string `json:"message_id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Replayed    bool   `json:"replayed"`
	DeliveryRef string `json:"delivery_id,omitempty"`
}

// DriverURLs are the loopback APIs of the drivers on this box.
var DriverURLs = map[string]string{"claude-code": "http://127.0.0.1:8792", "codex": "http://127.0.0.1:8788"}

// Who lists the sessions every reachable driver knows. A driver that does not
// answer is reported in skipped rather than failing the list.
func Who(ctx context.Context, urls map[string]string) (sessions []SessionInfo, skipped []string) {
	names := make([]string, 0, len(urls))
	for name := range urls {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var list struct {
			Sessions []SessionInfo `json:"sessions"`
		}
		if err := call(ctx, http.MethodGet, urls[name]+"/v1/sessions", nil, &list); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		sessions = append(sessions, list.Sessions...)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].SeenAt.After(sessions[j].SeenAt) })
	return sessions, skipped
}

// Resolve finds the recipient address for to: an exact crew address, or a
// unique prefix of an address or native session id among known sessions.
func Resolve(to string, sessions []SessionInfo) (string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", errors.New("--to is required")
	}
	var matches []string
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.Address == to {
			return to, nil
		}
		if (strings.HasPrefix(s.Address, to) || strings.HasPrefix(s.NativeID, to)) && !seen[s.Address] {
			seen[s.Address] = true
			matches = append(matches, s.Address)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if strings.HasPrefix(to, "s/") || strings.Contains(to, "/") {
			// An address no driver lists here, such as a pinned role address.
			return to, nil
		}
		return "", fmt.Errorf("no known session matches %q (see who)", to)
	}
	return "", fmt.Errorf("%q matches several sessions: %s", to, strings.Join(matches, ", "))
}

// Send asks the driver at url to send body from the caller's session.
func Send(ctx context.Context, url string, request SendRequest) (SendResult, error) {
	var result SendResult
	err := call(ctx, http.MethodPost, url+"/v1/send", request, &result)
	return result, err
}

// PrintWho writes the session list as a table.
func PrintWho(w io.Writer, sessions []SessionInfo, self string, skipped []string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ADDRESS\tRUNTIME\tSTATE\tSEEN\tSESSION\tCWD")
	for _, s := range sessions {
		mark := ""
		if s.NativeID == self {
			mark = " (you)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s%s\t%s\n", s.Address, s.Runtime, s.State, age(s.SeenAt), s.NativeID, mark, s.Cwd)
	}
	_ = tw.Flush()
	for _, s := range skipped {
		fmt.Fprintf(w, "not reachable: %s\n", s)
	}
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func call(ctx context.Context, method, url string, body, target any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return errors.New(resp.Status)
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(data, target)
}

// Main runs the send and who subcommands shared by the drivers. self is the
// caller's native session id from its runtime's environment ("" outside a
// session); ownURL is the calling runtime's driver.
func Main(ctx context.Context, args []string, self, ownURL string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: send [--policy next_turn|next_step|no_wake] --to ADDRESS TEXT... | who")
		return 2
	}
	switch args[0] {
	case "who":
		sessions, skipped := Who(ctx, DriverURLs)
		PrintWho(stdout, sessions, self, skipped)
		return 0
	case "send":
		return send(ctx, args[1:], self, ownURL, stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	return 2
}

func send(ctx context.Context, args []string, self, ownURL string, stdin io.Reader, stdout, stderr io.Writer) int {
	var to, policy, op string
	var text []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "--to" || a == "--policy" || a == "--operation-id") && i+1 < len(args):
			i++
			switch a {
			case "--to":
				to = args[i]
			case "--policy":
				policy = args[i]
			default:
				op = args[i]
			}
		case strings.HasPrefix(a, "--to="):
			to = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "--policy="):
			policy = strings.TrimPrefix(a, "--policy=")
		default:
			text = append(text, a)
		}
	}
	body := strings.Join(text, " ")
	if body == "" || body == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, 256*1024))
		if err != nil {
			fmt.Fprintln(stderr, "read message:", err)
			return 1
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "empty message")
		return 2
	}
	if self == "" {
		fmt.Fprintln(stderr, "not running inside a registered session (no session id in the environment)")
		return 1
	}
	sessions, _ := Who(ctx, DriverURLs)
	address, err := Resolve(to, sessions)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if op == "" {
		op = fmt.Sprintf("send/%s/%d", self, time.Now().UnixNano())
	}
	result, err := Send(ctx, ownURL, SendRequest{From: self, To: address, Body: body, Policy: policy, OperationID: op})
	if err != nil {
		fmt.Fprintln(stderr, "send:", err)
		return 1
	}
	fmt.Fprintf(stdout, "sent %s from %s to %s\n", result.MessageID, result.From, result.To)
	return 0
}
