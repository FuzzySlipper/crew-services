package crewcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"crew-services/internal/fabricclient"
	"crew-services/internal/store"
)

// SeatRequest is the body of a driver's POST /v1/seat-write: a seat write
// made by the caller's own session (From is its native session id). Write is
// take, leave, relieve, retire, charter or handoff.
type SeatRequest struct {
	From            string `json:"from"`
	Write           string `json:"write"`
	Seat            string `json:"seat"`
	Note            string `json:"note,omitempty"`
	Body            string `json:"body,omitempty"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
	OperationID     string `json:"operation_id"`
}

// SeatWriter is a driver making seat writes for its own sessions.
type SeatWriter func(ctx context.Context, request SeatRequest) (json.RawMessage, error)

// SeatWriteRequest builds the fabric request for a driver's session.
func SeatWriteRequest(request SeatRequest, adapterID, leaseToken, session string) (fabricclient.SeatWriteRequest, error) {
	if !slices.Contains(fabricclient.SeatWrites, request.Write) {
		return fabricclient.SeatWriteRequest{}, fmt.Errorf("unknown seat write %q", request.Write)
	}
	if request.Seat == "" || request.OperationID == "" {
		return fabricclient.SeatWriteRequest{}, errors.New("seat and operation_id are required")
	}
	return fabricclient.SeatWriteRequest{AdapterID: adapterID, LeaseToken: leaseToken, OperationID: request.OperationID, SessionAddress: session, Note: request.Note, Body: request.Body, ExpectedVersion: request.ExpectedVersion}, nil
}

// ServeSeatWrite is the handler of a driver's POST /v1/seat-write. A fabric
// refusal passes through with its own status and body, so the command line
// can say which seat rule refused it.
func ServeSeatWrite(write SeatWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body SeatRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		result, err := write(r.Context(), body)
		if err != nil {
			WriteDriverError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result)
	}
}

// WriteDriverError answers a driver API request that failed. A fabric refusal
// passes through with its own status and body; anything else is a 502.
func WriteDriverError(w http.ResponseWriter, err error) {
	var refused interface{ StatusBody() (int, string) }
	if errors.As(err, &refused) {
		status, text := refused.StatusBody()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, text)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// FabricURL is the crew-messaging API the command line reads seats from.
func FabricURL() string {
	if url := os.Getenv("CREW_FABRIC_URL"); url != "" {
		return url
	}
	return "http://127.0.0.1:8787"
}

// SeatsByHolder maps each holder session to the seats it holds, for who. It is
// best effort: an unreachable fabric just leaves the column empty.
func SeatsByHolder(ctx context.Context, fabricURL string) map[string][]string {
	client, err := fabricclient.New(fabricURL)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	seats, err := client.ListSeats(ctx, fabricclient.SeatFilter{})
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, s := range seats {
		if s.Holding != nil {
			name := s.Address
			if s.Holding.Stale {
				name += " (stale)"
			}
			out[s.Holding.HolderAddress] = append(out[s.Holding.HolderAddress], name)
		}
	}
	return out
}

const seatUsage = `usage: seat COMMAND
  seat list [--scope SCOPE] [--state vacant|held|stale|retired] [--mine]
  seat show SEAT
  seat take SEAT [--note TEXT]
  seat leave SEAT [--note TEXT]
  seat relieve SEAT [--note TEXT]
  seat retire SEAT [--note TEXT]
  seat charter SEAT [--version N]
  seat charter SEAT --set [--expect VERSION] [TEXT... | -]
  seat handoff SEAT [--version N]
  seat handoff SEAT --set [--expect VERSION] [TEXT... | -]
  seat versions SEAT charter|handoff
  seat holdings SEAT
Reads take --json for the fabric's own record.`

// seatArgs is a parsed seat command line.
type seatArgs struct {
	positional      []string
	note, scope     string
	state           string
	set, mine, json bool
	version, expect int64
	expectSet       bool
	operationID     string
}

func parseSeatArgs(args []string) (seatArgs, error) {
	var out seatArgs
	value := func(i *int, name string) (string, error) {
		a := args[*i]
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		var err error
		var v string
		switch name {
		case "--note":
			out.note, err = value(&i, name)
		case "--scope":
			out.scope, err = value(&i, name)
		case "--state":
			out.state, err = value(&i, name)
		case "--operation-id":
			out.operationID, err = value(&i, name)
		case "--version", "--expect":
			if v, err = value(&i, name); err == nil {
				var n int64
				if n, err = strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
					return out, fmt.Errorf("%s must be a version number", name)
				}
				if name == "--version" {
					out.version = n
				} else {
					out.expect, out.expectSet = n, true
				}
			}
		case "--set":
			out.set = true
		case "--mine":
			out.mine = true
		case "--json":
			out.json = true
		default:
			out.positional = append(out.positional, a)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func seatMain(ctx context.Context, args []string, self, ownURL string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, seatUsage)
		return 2
	}
	command := args[0]
	parsed, err := parseSeatArgs(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fabric, err := fabricclient.New(FabricURL())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	seat := ""
	if len(parsed.positional) > 0 {
		seat = parsed.positional[0]
	}
	needSeat := func() bool {
		if seat == "" {
			fmt.Fprintf(stderr, "seat %s needs a seat address (name@scope)\n", command)
			return false
		}
		return true
	}
	switch command {
	case "list":
		holder := ""
		if parsed.mine {
			if holder = ownAddress(ctx, self); holder == "" {
				fmt.Fprintln(stderr, "not running inside a registered session (see who)")
				return 1
			}
		}
		seats, err := fabric.ListSeats(ctx, fabricclient.SeatFilter{Scope: parsed.scope, State: parsed.state, Holder: holder})
		if err != nil {
			return seatFailed(stderr, "list seats", err)
		}
		if parsed.json {
			return printJSON(stdout, map[string]any{"seats": seats})
		}
		printSeats(stdout, seats, ownAddress(ctx, self))
		return 0
	case "show":
		if !needSeat() {
			return 2
		}
		value, err := fabric.GetSeat(ctx, seat)
		if err != nil {
			return seatFailed(stderr, "show "+seat, err)
		}
		if parsed.json {
			return printJSON(stdout, value)
		}
		printSeat(stdout, value)
		return 0
	case "versions":
		if !needSeat() {
			return 2
		}
		kind := "charter"
		if len(parsed.positional) > 1 {
			kind = parsed.positional[1]
		}
		values, err := fabric.ListSeatTexts(ctx, seat, kind)
		if err != nil {
			return seatFailed(stderr, kind+" versions of "+seat, err)
		}
		if parsed.json {
			return printJSON(stdout, map[string]any{"versions": values})
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "VERSION\tSET\tBY\tADAPTER\tAS SEATS\tLENGTH")
		for _, v := range values {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\n", v.Version, v.SetAt, v.SetByAddress, v.SetByAdapterID, strings.Join(v.SetBySeats, ","), v.Length)
		}
		_ = tw.Flush()
		fmt.Fprintf(stdout, "Read one with `seat %s %s --version N`.\n", kind, seat)
		return 0
	case "holdings":
		if !needSeat() {
			return 2
		}
		values, err := fabric.ListSeatHoldings(ctx, seat)
		if err != nil {
			return seatFailed(stderr, "holdings of "+seat, err)
		}
		if parsed.json {
			return printJSON(stdout, map[string]any{"holdings": values})
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "HOLDING\tHOLDER\tADAPTER\tSTARTED\tENDED\tREASON\tNOTE")
		for _, h := range values {
			ended, reason := h.EndedAt, h.EndReason
			if ended == "" {
				ended = "-"
				if h.Stale {
					reason = "stale: " + h.StaleReason
				}
			}
			note := h.EndNote
			if note == "" {
				note = h.StartNote
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", h.HoldingID, h.HolderAddress, h.HolderAdapterID, h.StartedAt, ended, reason, firstLine(note))
		}
		_ = tw.Flush()
		return 0
	case "charter", "handoff":
		if !needSeat() {
			return 2
		}
		if !parsed.set {
			value, err := fabric.GetSeatText(ctx, seat, command, parsed.version)
			if err != nil {
				return seatFailed(stderr, command+" of "+seat, err)
			}
			if parsed.json {
				return printJSON(stdout, value)
			}
			fmt.Fprintf(stdout, "%s %s version %d, set %s by %s\n\n%s\n", seat, command, value.Version, value.SetAt, setBy(value), value.Body)
			return 0
		}
		body := strings.Join(parsed.positional[1:], " ")
		if body == "" || body == "-" {
			data, err := io.ReadAll(stdin)
			if err != nil {
				fmt.Fprintln(stderr, "read text:", err)
				return 1
			}
			body = string(data)
		}
		request := SeatRequest{Write: command, Seat: seat, Body: body}
		if parsed.expectSet {
			request.ExpectedVersion = &parsed.expect
		}
		return seatWrite(ctx, request, parsed, self, ownURL, stdout, stderr)
	case "take", "leave", "relieve", "retire":
		if !needSeat() {
			return 2
		}
		return seatWrite(ctx, SeatRequest{Write: command, Seat: seat, Note: parsed.note}, parsed, self, ownURL, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown seat command %q\n%s\n", command, seatUsage)
	return 2
}

func seatWrite(ctx context.Context, request SeatRequest, parsed seatArgs, self, ownURL string, stdout, stderr io.Writer) int {
	if self == "" {
		fmt.Fprintln(stderr, "not running inside a registered session (no session id in the environment)")
		return 1
	}
	request.From = self
	request.OperationID = parsed.operationID
	if request.OperationID == "" {
		request.OperationID = fmt.Sprintf("seat/%s/%s/%d", request.Write, self, time.Now().UnixNano())
	}
	var result struct {
		Seat            fabricclient.Seat      `json:"seat"`
		Holding         *fabricclient.Holding  `json:"holding"`
		PreviousHolding *fabricclient.Holding  `json:"previous_holding"`
		Text            *fabricclient.SeatText `json:"text"`
		Replayed        bool                   `json:"replayed"`
	}
	if err := call(ctx, http.MethodPost, ownURL+"/v1/seat-write", request, &result); err != nil {
		fmt.Fprintf(stderr, "seat %s %s: %v\n", request.Write, request.Seat, err)
		return 1
	}
	if parsed.json {
		return printJSON(stdout, result)
	}
	seat := request.Seat
	switch request.Write {
	case "take":
		fmt.Fprintf(stdout, "you hold %s (holding %d).\n", seat, result.Holding.HoldingID)
		if result.Seat.CharterVersion == 0 {
			fmt.Fprintf(stdout, "It has no charter yet: write one with `seat charter %s --set`.\n", seat)
		}
		fmt.Fprintf(stdout, "Read its charter and handoff with `seat show %s`; mail to %s now comes to you, and `send --as %s` sends as the seat.\n", seat, seat, seat)
	case "relieve":
		previous := "nobody"
		if result.PreviousHolding != nil {
			previous = result.PreviousHolding.HolderAddress
		}
		fmt.Fprintf(stdout, "you hold %s (holding %d), relieving %s. Read the handoff with `seat show %s`.\n", seat, result.Holding.HoldingID, previous, seat)
	case "leave":
		fmt.Fprintf(stdout, "you left %s; it is vacant and its mail waits for the next holder.\n", seat)
		if result.PreviousHolding != nil && result.PreviousHolding.EndReason == store.HoldingLeftWithoutHandoff {
			fmt.Fprintf(stdout, "You wrote no handoff while you held it; the next holder sees the previous one. `seat handoff %s --set` still works for anyone.\n", seat)
		}
	case "retire":
		fmt.Fprintf(stdout, "retired %s; any mail still queued for it failed with seat_retired.\n", seat)
	default:
		fmt.Fprintf(stdout, "set the %s of %s to version %d (%d characters).\n", request.Write, seat, result.Text.Version, result.Text.Length)
	}
	if result.Replayed {
		fmt.Fprintln(stdout, "(replayed an earlier request with the same operation id)")
	}
	return 0
}

func ownAddress(ctx context.Context, self string) string {
	if self == "" {
		return ""
	}
	sessions, _ := Who(ctx, DriverURLs)
	for _, s := range sessions {
		if s.NativeID == self {
			return s.Address
		}
	}
	return ""
}

func printSeats(w io.Writer, seats []fabricclient.Seat, self string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SEAT\tSTATE\tHOLDER\tCHARTER\tHANDOFF\tQUEUED\tCHARTER SUMMARY")
	for _, s := range seats {
		holder := "-"
		if s.Holding != nil {
			holder = s.Holding.HolderAddress
			if holder == self {
				holder += " (you)"
			}
			if s.Holding.Stale {
				holder += " stale: " + s.Holding.StaleReason
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\tv%d\tv%d\t%d\t%s\n", s.Address, s.State, holder, s.CharterVersion, s.HandoffVersion, s.QueuedMail, s.CharterSummary)
	}
	_ = tw.Flush()
}

func printSeat(w io.Writer, s fabricclient.Seat) {
	fmt.Fprintf(w, "%s: %s, generation %d, created %s by %s, %d queued\n", s.Address, s.State, s.Generation, s.CreatedAt, s.CreatedBy, s.QueuedMail)
	if h := s.Holding; h != nil {
		fmt.Fprintf(w, "held by %s (%s) since %s, holding %d", h.HolderAddress, h.HolderAdapterID, h.StartedAt, h.HoldingID)
		if h.Stale {
			fmt.Fprintf(w, "; stale: %s (relieve it with `seat relieve %s`)", h.StaleReason, s.Address)
		}
		fmt.Fprintln(w)
	}
	for _, text := range []struct {
		kind  string
		value *fabricclient.SeatText
	}{{"charter", s.Charter}, {"handoff", s.Handoff}} {
		if text.value == nil {
			fmt.Fprintf(w, "\n%s: none yet\n", strings.ToUpper(text.kind[:1])+text.kind[1:])
			continue
		}
		fmt.Fprintf(w, "\n%s (version %d, set %s by %s):\n%s\n", strings.ToUpper(text.kind[:1])+text.kind[1:], text.value.Version, text.value.SetAt, setBy(*text.value), text.value.Body)
	}
}

func setBy(t fabricclient.SeatText) string {
	by := t.SetByAddress + " (" + t.SetByAdapterID + ")"
	if len(t.SetBySeats) > 0 {
		by += " holding " + strings.Join(t.SetBySeats, ", ")
	}
	return by
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 60 {
		line = line[:57] + "..."
	}
	return line
}

func printJSON(w io.Writer, value any) int {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
	return 0
}

func seatFailed(stderr io.Writer, what string, err error) int {
	fmt.Fprintf(stderr, "%s: %s\n", what, describeFabricError(err))
	return 1
}

// describeFabricError turns a fabric refusal body into a sentence.
func describeFabricError(err error) string {
	var typed *fabricclient.HTTPError
	if !errors.As(err, &typed) {
		return err.Error()
	}
	if text := describeRefusal([]byte(typed.Body)); text != "" {
		return text
	}
	return err.Error()
}

// describeRefusal reads a fabric error body: its code and, for seat refusals,
// the holding or version in the way.
func describeRefusal(body []byte) string {
	var e struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Detail struct {
			Holding        *fabricclient.Holding `json:"holding"`
			CurrentVersion *int64                `json:"current_version"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil || (e.Code == "" && e.Error == "") {
		return ""
	}
	if e.Code == "" {
		return e.Error
	}
	text := e.Code
	switch {
	case e.Detail.Holding != nil:
		h := e.Detail.Holding
		text += fmt.Sprintf(": held by %s (%s) since %s", h.HolderAddress, h.HolderAdapterID, h.StartedAt)
		if h.Stale {
			text += ", stale: " + h.StaleReason
		}
		text += "; `seat relieve` takes it over and tells them"
	case e.Code == "not_holder":
		text += ": this session does not hold that seat now; `seat show` names its holder"
	case e.Detail.CurrentVersion != nil:
		text += fmt.Sprintf(": the current version is %d; read it, then set again with --expect %d", *e.Detail.CurrentVersion, *e.Detail.CurrentVersion)
	}
	if e.Error != "" {
		text += ": " + e.Error
	}
	return text
}

// SeatContext is what a session is told at start about the seats it holds,
// or "" when it holds none.
func SeatContext(command string, seats []string) string {
	if len(seats) == 0 {
		return ""
	}
	return fmt.Sprintf(" You hold the crew seat%s %s: mail to a seat comes to you, and `%s send --as SEAT` sends as it. Read each seat's charter and handoff with `%s seat show SEAT` before acting for it, and write the handoff with `%s seat handoff SEAT --set` before you leave it.", map[bool]string{true: "s", false: ""}[len(seats) > 1], strings.Join(seats, ", "), command, command, command)
}
