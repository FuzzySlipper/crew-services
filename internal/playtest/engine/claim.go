package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"crew-services/internal/playtest/input"
)

const (
	// The context every C# product runtime declares for gameplay input.
	gameplayContext = "gameplay.default"
	// The claim lapses this long after the harness's last input (on realtime
	// ticks, which run while time is held), handing input back to the page.
	claimLease = 5 * time.Minute
)

// fact is one wire input fact: {"kind":"key","code":"key-w","edge":"pressed"}.
type fact map[string]any

// claim holds the harness's input claim on one host. Input under a claim
// reaches the runtime's input lane directly: binding admission and the
// product's mappings, not the page's input capture.
type claim struct {
	host  *Host
	label string

	stream  *watcher
	binding *Binding
	next    uint64
	// lastSent is the sequence of the newest event this claim posted.
	lastSent uint64
	// uncertain forces a fresh claim: a batch's outcome is unknown, so it is
	// never replayed, and the new claim clears whatever it left held.
	uncertain string
	held      map[string]fact
}

// Delivery is one batch's receipt. Queued is the host's acknowledgement;
// Admitted is the runtime ingesting it, as its output stream reported.
type Delivery struct {
	Events     int    `json:"events"`
	Delivery   string `json:"delivery"`
	Code       string `json:"code,omitempty"`
	Through    uint64 `json:"sequenceThrough,omitempty"`
	Admitted   string `json:"admission,omitempty"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

func (c *claim) status() map[string]any {
	state := map[string]any{"label": c.label, "held_controls": len(c.held)}
	if c.binding != nil {
		state["binding"] = c.binding
		state["next_sequence"] = c.next
	}
	if c.uncertain != "" {
		state["uncertain"] = c.uncertain
	}
	if c.stream != nil {
		p := c.stream.snapshot()
		state["stream_connected"] = p.connected
		state["published_claim"] = p.claimLabel
	}
	return state
}

// ensure holds a live claim, claiming the runtime's current binding if this
// claim lapsed, was released, was replaced by a restage, or is uncertain.
func (c *claim) ensure(ctx context.Context) error {
	if c.stream == nil {
		c.stream = watch(c.host)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	current, ok := c.stream.wait(waitCtx, func(p published) bool { return p.binding != nil })
	if !ok {
		return fmt.Errorf("input binding unavailable: the host's output stream has not published one (%s)", current.err)
	}
	if c.uncertain == "" && c.binding != nil && *current.binding == *c.binding && current.claimLabel == c.label {
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := c.host.Claim(ctx, *current.binding, c.label, claimLease)
		if err == nil && result.Accepted && result.Binding != nil {
			next, parseErr := strconv.ParseUint(result.NextInputSequence, 10, 64)
			if parseErr != nil {
				return fmt.Errorf("claim answered an invalid nextInputSequence %q", result.NextInputSequence)
			}
			c.binding, c.next, c.uncertain, c.held = result.Binding, next, "", map[string]fact{}
			return nil
		}
		if err != nil && result.Commit != "not-applied" {
			c.uncertain = "claim outcome unknown: " + err.Error()
			return fmt.Errorf("input claim outcome unknown; nothing was sent: %w", err)
		}
		// A refused claim names a binding that moved meanwhile: wait for the
		// stream to publish the newer one and claim that.
		stale := *current.binding
		current, ok = c.stream.wait(waitCtx, func(p published) bool { return p.binding != nil && *p.binding != stale })
		if !ok {
			return fmt.Errorf("input claim refused: %s %s", result.Code, result.Diagnostic)
		}
	}
	return errors.New("input claim refused twice; the binding keeps moving")
}

// send posts facts as one ordered batch under the claim.
func (c *claim) send(ctx context.Context, facts []fact) (Delivery, error) {
	if len(facts) == 0 {
		return Delivery{Delivery: "nothing to send"}, nil
	}
	if err := c.ensure(ctx); err != nil {
		return Delivery{Events: len(facts), Delivery: "not-sent"}, err
	}
	batch := make([]map[string]any, len(facts))
	for i, f := range facts {
		batch[i] = map[string]any{"runtime": c.binding, "sequence": strconv.FormatUint(c.next+uint64(i), 10), "context": gameplayContext, "fact": f}
	}
	result, err := c.host.Input(ctx, batch)
	receipt := Delivery{Events: len(facts), Code: result.Code, Diagnostic: result.Diagnostic}
	switch {
	case err != nil && result.Commit == "not-applied":
		receipt.Delivery = "not-applied"
		return receipt, err
	case err != nil || result.Commit == "unknown":
		c.uncertain = "input outcome unknown"
		receipt.Delivery = "uncertain; reobserve without replay"
		if err == nil {
			err = fmt.Errorf("input outcome unknown: %s %s", result.Code, result.Diagnostic)
		}
		return receipt, err
	case result.Commit == "resync-required":
		c.uncertain = "host asked for a resync: " + result.Code
		receipt.Delivery = "resync-required; held input cleared, reobserve without replay"
		return receipt, fmt.Errorf("input needs a resync: %s", result.Code)
	case !result.Accepted:
		// Stale or out-of-order batches are rolled back or dropped whole.
		c.uncertain = "input refused: " + result.Code
		receipt.Delivery = "not-applied"
		return receipt, fmt.Errorf("input refused: %s %s", result.Code, result.Diagnostic)
	}
	c.next += uint64(len(facts))
	c.lastSent = c.next - 1
	receipt.Through = c.lastSent
	receipt.Delivery = "queued"
	if result.AcceptedThrough != "" {
		receipt.Delivery, receipt.Admitted = "admitted", "runtime receipt"
	}
	for _, f := range facts {
		if id, edge := heldID(f); id != "" {
			if edge == "pressed" {
				c.held[id] = f
			} else {
				delete(c.held, id)
			}
		}
	}
	return receipt, nil
}

// admitted waits briefly for the runtime to report ingesting this claim's
// newest event. Admission happens at the next realtime tick or before the
// next debug command; it is not product observation.
func (c *claim) admitted(ctx context.Context, wait time.Duration) string {
	if c.stream == nil || c.binding == nil {
		return "unobserved"
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	binding, through := *c.binding, c.lastSent
	_, ok := c.stream.wait(ctx, func(p published) bool { return p.admittedBinding == binding && p.admitted >= through })
	if ok {
		return "admitted by the runtime"
	}
	return "queued; not yet admitted (in held time the runtime takes queued input with the next advance or debug command)"
}

// releaseHeld lifts every control this claim still holds.
func (c *claim) releaseHeld(ctx context.Context) error {
	if len(c.held) == 0 || c.uncertain != "" {
		return nil
	}
	var facts []fact
	for _, f := range c.held {
		released := fact{}
		for k, v := range f {
			released[k] = v
		}
		released["edge"] = "released"
		facts = append(facts, released)
	}
	_, err := c.send(ctx, facts)
	return err
}

// release hands input back to the page. The runtime clears what was held.
func (c *claim) release(ctx context.Context) (map[string]any, error) {
	defer func() {
		if c.stream != nil {
			c.stream.stop()
			c.stream = nil
		}
	}()
	if c.binding == nil {
		return map[string]any{"released": false, "reason": "no claim was taken"}, nil
	}
	binding := *c.binding
	c.binding = nil
	result, err := c.host.ReleaseClaim(ctx, binding)
	if err != nil {
		return map[string]any{"released": false, "commit": result.Commit}, err
	}
	if !result.Accepted {
		// The claim had already lapsed or the runtime restaged: nothing is held.
		return map[string]any{"released": false, "reason": result.Code + ": claim no longer current"}, nil
	}
	return map[string]any{"released": true, "binding": result.Binding}, nil
}

func heldID(f fact) (string, string) {
	edge, _ := f["edge"].(string)
	switch f["kind"] {
	case "key":
		return "key:" + fmt.Sprint(f["code"]), edge
	case "pointer-button", "controller-button":
		return fmt.Sprint(f["kind"], ":", f["button"]), edge
	}
	return "", ""
}

// keyCode turns a DOM KeyboardEvent.code ("KeyW", "Digit1", "ShiftLeft",
// "ArrowUp", "Space") into the Engine's wire code ("key-w", "digit-1", ...).
func keyCode(dom string) (string, error) {
	var out strings.Builder
	for i, r := range dom {
		if unicode.IsUpper(r) || unicode.IsDigit(r) && i > 0 && !unicode.IsDigit(rune(dom[i-1])) {
			if i > 0 {
				out.WriteByte('-')
			}
		}
		out.WriteRune(unicode.ToLower(r))
	}
	code := out.String()
	if !wireKeys[code] {
		return "", fmt.Errorf("control %q has no Engine wire key", dom)
	}
	return code, nil
}

var wireKeys = func() map[string]bool {
	keys := map[string]bool{"space": true, "enter": true, "escape": true, "shift-left": true, "shift-right": true, "control-left": true,
		"control-right": true, "alt-left": true, "alt-right": true, "arrow-up": true, "arrow-down": true, "arrow-left": true, "arrow-right": true}
	for c := 'a'; c <= 'z'; c++ {
		keys["key-"+string(c)] = true
	}
	for c := '0'; c <= '9'; c++ {
		keys["digit-"+string(c)] = true
	}
	return keys
}()

// pointerButton maps a product action's pointer control to the wire button.
var pointerButton = map[string]string{"Primary": "primary", "Secondary": "secondary", "Auxiliary": "middle"}

// controlFact is the pressed edge of a product action's control.
func controlFact(control string) (fact, error) {
	if button, ok := pointerButton[control]; ok {
		return fact{"kind": "pointer-button", "button": button, "edge": "pressed"}, nil
	}
	code, err := keyCode(control)
	if err != nil {
		return nil, err
	}
	return fact{"kind": "key", "code": code, "edge": "pressed"}, nil
}

func released(f fact) fact {
	out := fact{}
	for k, v := range f {
		out[k] = v
	}
	out["edge"] = "released"
	return out
}

// virtualKey maps the raw input batch's Windows virtual-key codes.
func virtualKey(vk int) (string, error) {
	switch {
	case vk >= 'A' && vk <= 'Z':
		return "key-" + string(rune(vk+'a'-'A')), nil
	case vk >= '0' && vk <= '9':
		return "digit-" + string(rune(vk)), nil
	}
	codes := map[int]string{0x20: "space", 0x0D: "enter", 0x1B: "escape", 0x10: "shift-left", 0xA0: "shift-left", 0xA1: "shift-right",
		0x11: "control-left", 0xA2: "control-left", 0xA3: "control-right", 0x12: "alt-left", 0xA4: "alt-left", 0xA5: "alt-right",
		0x25: "arrow-left", 0x26: "arrow-up", 0x27: "arrow-right", 0x28: "arrow-down"}
	if code, ok := codes[vk]; ok {
		return code, nil
	}
	return "", fmt.Errorf("virtual key %d has no Engine wire key", vk)
}

// Standard-gamepad button indices, as the page's Gamepad API reports them.
var gamepadButton = map[string]int{"a": 0, "b": 1, "x": 2, "y": 3, "lb": 4, "rb": 5, "back": 8, "start": 9, "ls": 10, "rs": 11, "up": 12, "down": 13, "left": 14, "right": 15}

// steps delivers a validated raw batch in wall time.
func (c *claim) steps(ctx context.Context, steps []map[string]any) (map[string]any, error) {
	if err := input.ValidateBatch(steps); err != nil {
		return nil, err
	}
	var receipts []Delivery
	completed := 0
	fail := func(err error) (map[string]any, error) {
		releaseErr := c.releaseHeld(context.WithoutCancel(ctx))
		result := map[string]any{"completed_steps": completed, "deliveries": receipts, "input_path": "claimed runtime input"}
		if releaseErr != nil {
			result["release_error"] = releaseErr.Error()
		}
		return result, err
	}
	pause := func(ms int) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(ms) * time.Millisecond):
			return nil
		}
	}
	for _, step := range steps {
		ms := number(step["ms"])
		var press []fact
		switch step["kind"] {
		case "wait":
			if err := pause(ms); err != nil {
				return fail(err)
			}
			completed++
			continue
		case "hold":
			for _, key := range step["keys"].([]any) {
				code, err := virtualKey(number(key))
				if err != nil {
					return fail(err)
				}
				press = append(press, fact{"kind": "key", "code": code, "edge": "pressed"})
			}
		case "move":
			press = []fact{{"kind": "pointer-delta", "x": number(step["dx"]), "y": number(step["dy"])}}
		case "click":
			button, _ := step["button"].(string)
			wire := map[string]string{"": "primary", "left": "primary", "right": "secondary", "middle": "middle"}[button]
			if wire == "" {
				return fail(fmt.Errorf("click button %q is not left, right or middle", button))
			}
			press = []fact{{"kind": "pointer-button", "button": wire, "edge": "pressed"}}
		case "gamepad":
			var err error
			if press, err = gamepadFacts(step); err != nil {
				return fail(err)
			}
		case "point":
			return fail(errors.New("capability_unavailable: absolute pointer positions need a page; use move for relative motion"))
		}
		receipt, err := c.send(ctx, press)
		receipts = append(receipts, receipt)
		if err != nil {
			return fail(err)
		}
		if err := pause(ms); err != nil {
			return fail(err)
		}
		var lift []fact
		for _, f := range press {
			switch f["kind"] {
			case "key", "pointer-button", "controller-button":
				lift = append(lift, released(f))
			case "controller-axis":
				lift = append(lift, fact{"kind": "controller-axis", "axis": f["axis"], "value": 0})
			case "controller-button-value":
				lift = append(lift, fact{"kind": "controller-button-value", "button": f["button"], "value": 0})
			}
		}
		if len(lift) > 0 {
			receipt, err := c.send(ctx, lift)
			receipts = append(receipts, receipt)
			if err != nil {
				return fail(err)
			}
		}
		completed++
	}
	return map[string]any{"completed_steps": completed, "deliveries": receipts, "admission": c.admitted(ctx, time.Second),
		"input_path": "claimed runtime input (binding admission and product mappings; not page input capture)"}, nil
}

func gamepadFacts(step map[string]any) ([]fact, error) {
	var facts []fact
	if buttons, ok := step["buttons"].([]any); ok {
		for _, name := range buttons {
			index, ok := gamepadButton[fmt.Sprint(name)]
			if !ok {
				return nil, fmt.Errorf("gamepad button %v has no Engine wire button", name)
			}
			facts = append(facts, fact{"kind": "controller-button", "button": fmt.Sprintf("button-%d", index), "edge": "pressed"})
		}
	}
	// Harness sticks use positive Y for up; the standard gamepad uses negative.
	for i, axis := range []struct {
		name string
		sign float64
	}{{"lx", 1}, {"ly", -1}, {"rx", 1}, {"ry", -1}} {
		if value := real(step[axis.name]); value != 0 {
			facts = append(facts, fact{"kind": "controller-axis", "axis": fmt.Sprintf("axis-%d", i), "value": axis.sign * value})
		}
	}
	for name, index := range map[string]int{"lt": 6, "rt": 7} {
		if value := real(step[name]); value != 0 {
			facts = append(facts, fact{"kind": "controller-button-value", "button": fmt.Sprintf("button-%d", index), "value": value})
		}
	}
	if len(facts) == 0 {
		return nil, errors.New("gamepad step sets no button, stick or trigger")
	}
	return facts, nil
}

func number(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case interface{ Int64() (int64, error) }:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func real(value any) float64 {
	switch v := value.(type) {
	case int:
		return float64(v)
	case float64:
		return v
	case interface{ Float64() (float64, error) }:
		f, _ := v.Float64()
		return f
	}
	return 0
}
