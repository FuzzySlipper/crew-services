package target

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeWolf struct {
	mu             sync.Mutex
	sessions       []map[string]any
	lobbies        []map[string]any
	packets        [][]byte
	pressed        chan struct{}
	releaseStarted chan struct{}
	releaseGate    chan struct{}
	afterSessions  func()
	created        []map[string]any
	joined         []map[string]any
	stopped        []string
	failJoin       bool
	failCreate     bool
	failDown       bool
	failAll        bool
	pairedClient   string
}

func (f *fakeWolf) Call(_ context.Context, route string, data any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch route {
	case "clients":
		clientID := f.pairedClient
		if clientID == "" {
			clientID = "our-client"
		}
		return map[string]any{"success": true, "clients": []any{map[string]any{"client_id": clientID}}}, nil
	case "profiles":
		return map[string]any{
			"success": true,
			"profiles": []any{map[string]any{
				"id": "user",
				"apps": []any{map[string]any{
					"title": "Firefox", "icon_png_path": "/wolf/firefox.png", "render_node": "/dev/dri/renderD128",
					"runner": map[string]any{"type": "docker", "name": "Firefox", "image": "wolf-firefox:current", "env": []any{"ONE=1"}, "mounts": []any{"/dev/input:/dev/input:ro", "/tmp/keep:/keep:ro"}},
				}},
			}},
		}, nil
	case "sessions":
		if f.afterSessions != nil {
			f.afterSessions()
			f.afterSessions = nil
		}
		return map[string]any{"success": true, "sessions": maps(f.sessions)}, nil
	case "lobbies":
		return map[string]any{"success": true, "lobbies": maps(f.lobbies)}, nil
	case "sessions/input":
		request := data.(map[string]any)
		raw, err := hex.DecodeString(request["input_packet_hex"].(string))
		if err != nil {
			return nil, err
		}
		f.packets = append(f.packets, raw)
		kind := binary.LittleEndian.Uint32(raw[8:])
		if kind == 3 || kind == 12 {
			select {
			case <-f.pressed:
			default:
				close(f.pressed)
			}
		}
		if f.failAll || (f.failDown && (kind == 3 || kind == 12)) {
			return nil, errors.New("Acknowledgement lost after delivery")
		}
		if kind == 4 && f.releaseStarted != nil {
			select {
			case <-f.releaseStarted:
			default:
				close(f.releaseStarted)
			}
			<-f.releaseGate
		}
		return map[string]any{"success": true}, nil
	case "lobbies/create":
		request := data.(map[string]any)
		f.created = append(f.created, request)
		id := "created-lobby"
		f.lobbies = append(f.lobbies, map[string]any{"id": id, "name": request["name"], "connected_sessions": []any{}})
		if f.failCreate {
			return nil, errors.New("lobby setup timed out")
		}
		return map[string]any{"success": true, "lobby_id": id}, nil
	case "lobbies/join":
		request := data.(map[string]any)
		f.joined = append(f.joined, request)
		if f.failJoin {
			return nil, errors.New("join failed")
		}
		for _, lobby := range f.lobbies {
			if lobby["id"] == request["lobby_id"] {
				lobby["connected_sessions"] = append(lobby["connected_sessions"].([]any), request["moonlight_session_id"])
			}
		}
		return map[string]any{"success": true}, nil
	case "sessions/stop":
		request := data.(map[string]any)
		id := request["session_id"].(string)
		f.sessions = withoutClient(f.sessions, id)
		return map[string]any{"success": true}, nil
	case "lobbies/stop":
		request := data.(map[string]any)
		id := request["lobby_id"].(string)
		f.stopped = append(f.stopped, id)
		next := []map[string]any{}
		for _, lobby := range f.lobbies {
			if lobby["id"] != id {
				next = append(next, lobby)
			}
		}
		f.lobbies = next
		return map[string]any{"success": true}, nil
	default:
		return nil, errors.New("unexpected Wolf route: " + route)
	}
}

func maps(source []map[string]any) []any {
	result := make([]any, len(source))
	for i := range source {
		result[i] = source[i]
	}
	return result
}
func withoutClient(items []map[string]any, id string) []map[string]any {
	next := []map[string]any{}
	for _, item := range items {
		if item["client_id"] != id {
			next = append(next, item)
		}
	}
	return next
}

func newTarget(t *testing.T) (*Controller, *fakeWolf, string) {
	t.Helper()
	fake := &fakeWolf{pressed: make(chan struct{}), sessions: []map[string]any{{"client_id": "another"}}}
	controller, err := NewController(fake, t.TempDir(), "our-client")
	if err != nil {
		t.Fatal(err)
	}
	controller.SetVideoProducerBufferCaps("video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}")
	lease, err := controller.Dispatch(context.Background(), map[string]any{"op": "acquire"})
	if err != nil {
		t.Fatal(err)
	}
	fake.sessions = append(fake.sessions, map[string]any{"client_id": "our-client", "video_width": 1280, "video_height": 720, "video_refresh_rate": 60, "audio_channel_count": 2, "client_settings": map[string]any{"mouse_acceleration": 1}})
	return controller, fake, lease["id"].(string)
}

func TestGamepadPacketMatchesPythonDonorVector(t *testing.T) {
	packet, err := gamepadPacket(map[string]any{"lx": .5, "rt": 1, "buttons": []any{"a", "lb"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	const want = "060222000000001e0c0000001a00000001001400001100ff00400000000000009c0000005500"
	if packet != want {
		t.Fatalf("packet = %s, want %s", packet, want)
	}
	neutral, err := gamepadPacket(map[string]any{}, true)
	if err != nil {
		t.Fatal(err)
	}
	const neutralWant = "060222000000001e0c0000001a000000010014000000000000000000000000009c0000005500"
	if neutral != neutralWant {
		t.Fatalf("neutral = %s, want %s", neutral, neutralWant)
	}
}

func TestGamepadPacketUsesFirefoxStandardXboxFaceButtons(t *testing.T) {
	buttons := map[string]uint16{"a": 0x1000, "b": 0x2000, "x": 0x8000, "y": 0x4000, "lb": 0x0100, "rb": 0x0200}
	for name, want := range buttons {
		packet, err := gamepadPacket(map[string]any{"buttons": []any{name}}, true)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := hex.DecodeString(packet)
		if err != nil {
			t.Fatal(err)
		}
		if got := binary.LittleEndian.Uint16(raw[20:22]); got != want {
			t.Fatalf("%s Wolf gamepad button bits = %#04x, want %#04x", name, got, want)
		}
	}

	packet, err := gamepadPacket(map[string]any{"lt": .25, "rt": .75}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(packet)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := raw[22], byte(64); got != want {
		t.Fatalf("left trigger = %d, want %d", got, want)
	}
	if got, want := raw[23], byte(191); got != want {
		t.Fatalf("right trigger = %d, want %d", got, want)
	}
}

func TestInvalidBatchAndPreArrivalCancellationDoNotDeliver(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	_, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{
		map[string]any{"kind": "hold", "keys": []any{87}, "ms": 100},
		map[string]any{"kind": "move", "dx": 999999, "dy": 0},
	}})
	if err == nil {
		t.Fatal("invalid later step was accepted")
	}
	if len(wolf.packets) != 0 {
		t.Fatalf("invalid batch delivered %d packets", len(wolf.packets))
	}
	_, err = controller.Dispatch(context.Background(), map[string]any{"op": "cancel", "lease_id": lease, "action_id": "delayed"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "action_id": "delayed", "steps": []any{map[string]any{"kind": "hold", "keys": []any{87}, "ms": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if result["cancelled"] != true {
		t.Fatalf("receipt = %#v", result)
	}
	if len(wolf.packets) != 0 {
		t.Fatalf("pre-cancelled action delivered %d packets", len(wolf.packets))
	}
}

func TestGamepadRejectsUnknownButtonFieldBeforeDelivery(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	_, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "gamepad", "back": true, "ms": 120}}})
	if err == nil || !strings.Contains(err.Error(), "buttons:['back']") {
		t.Fatalf("gamepad back field error = %v", err)
	}
	wolf.mu.Lock()
	packetCount := len(wolf.packets)
	wolf.mu.Unlock()
	if packetCount != 0 {
		t.Fatalf("unknown gamepad field delivered %d packets", packetCount)
	}
}

func TestCancellationReleasesUncertainKeyDown(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	wolf.failDown = true
	result, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "hold", "keys": []any{87}, "ms": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if result["error"] == nil {
		t.Fatalf("missing lost delivery receipt: %#v", result)
	}
	if len(wolf.packets) != 2 {
		t.Fatalf("packets = %d, want down and release", len(wolf.packets))
	}
	if binary.LittleEndian.Uint32(wolf.packets[0][8:]) != 3 || binary.LittleEndian.Uint32(wolf.packets[1][8:]) != 4 {
		t.Fatalf("unexpected packet kinds")
	}
}

func TestExpiryCleansOwnedResourcesAndFailureRetainsLease(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	wolf.lobbies = []map[string]any{
		{"id": "ours", "name": "ours", "connected_sessions": []any{"our-client"}},
		{"id": "other", "name": "other", "connected_sessions": []any{"another"}},
	}
	controller.mu.Lock()
	controller.lease.ExpiresAt = 0
	controller.mu.Unlock()
	if err := controller.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	wolf.mu.Lock()
	remainingLobbies := len(wolf.lobbies)
	remainingSessions := len(wolf.sessions)
	wolf.mu.Unlock()
	if remainingLobbies != 1 || remainingSessions != 1 {
		t.Fatalf("expiry cleanup kept lobbies=%d sessions=%d", remainingLobbies, remainingSessions)
	}
	controller.mu.Lock()
	cleared := controller.lease == nil
	controller.mu.Unlock()
	if !cleared {
		t.Fatal("lease was not cleared after successful expiry cleanup")
	}

	controller, wolf, lease = newTarget(t)
	wolf.failAll = true
	controller.mu.Lock()
	controller.lease.Gamepad = true
	controller.lease.ExpiresAt = 0
	controller.mu.Unlock()
	if err := controller.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	retained := controller.lease != nil && controller.lease.ID == lease && controller.lease.Closing
	controller.mu.Unlock()
	if !retained {
		t.Fatal("cleanup failure did not retain closing lease")
	}
}

func TestCancelDuringHoldReturnsAfterRelease(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	result := make(chan map[string]any, 1)
	go func() {
		value, _ := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "hold", "keys": []any{17, 87}, "ms": 8000}}})
		result <- value
	}()
	select {
	case <-wolf.pressed:
	case <-time.After(time.Second):
		t.Fatal("key down not delivered")
	}
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "cancel", "lease_id": lease}); err != nil {
		t.Fatal(err)
	}
	select {
	case receipt := <-result:
		if receipt["cancelled"] != true {
			t.Fatalf("receipt = %#v", receipt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input did not finish after cancellation")
	}
	if len(wolf.packets) != 4 {
		t.Fatalf("packets = %d, want two downs and releases", len(wolf.packets))
	}
}

func TestSecondInputCannotOverlapNeutralization(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	wolf.releaseStarted = make(chan struct{})
	wolf.releaseGate = make(chan struct{})
	first := make(chan map[string]any, 1)
	go func() {
		result, _ := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "hold", "keys": []any{87}, "ms": 8000}}})
		first <- result
	}()
	select {
	case <-wolf.pressed:
	case <-time.After(time.Second):
		t.Fatal("key down not delivered")
	}
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "cancel", "lease_id": lease}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wolf.releaseStarted:
	case <-time.After(time.Second):
		t.Fatal("key release did not begin")
	}
	second := make(chan error, 1)
	go func() {
		_, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "wait", "ms": 0}}})
		second <- err
	}()
	select {
	case err := <-second:
		if err == nil || err.Error() != "Input batch already running" {
			t.Fatalf("second input error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second input blocked instead of observing active cleanup")
	}
	close(wolf.releaseGate)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first input did not finish after release unblocked")
	}
}

func TestGamepadOwnershipPersistenceFailurePreventsDelivery(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	leasePath := filepath.Join(controller.state, stateLease)
	wolf.afterSessions = func() {
		if err := os.Remove(leasePath); err != nil {
			t.Fatalf("remove lease file: %v", err)
		}
		if err := os.Mkdir(leasePath, 0o700); err != nil {
			t.Fatalf("make lease obstruction: %v", err)
		}
	}
	result, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "gamepad", "buttons": []any{"a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result["error"].(string), "persist gamepad ownership") {
		t.Fatalf("receipt = %#v", result)
	}
	wolf.mu.Lock()
	packetCount := len(wolf.packets)
	wolf.mu.Unlock()
	if packetCount != 0 {
		t.Fatalf("gamepad persistence failure delivered %d packets", packetCount)
	}
	controller.mu.Lock()
	persistedOwnership := controller.lease.Gamepad
	controller.mu.Unlock()
	if persistedOwnership {
		t.Fatal("failed gamepad ownership update remained in memory")
	}
}

func TestReceiptAndReleaseReportPersistenceFailures(t *testing.T) {
	controller, _, lease := newTarget(t)
	eventsPath := filepath.Join(controller.state, "events.jsonl")
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.Dispatch(context.Background(), map[string]any{"op": "input", "lease_id": lease, "steps": []any{map[string]any{"kind": "wait", "ms": 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt["evidence_error"].(string), "record input receipt") {
		t.Fatalf("receipt = %#v", receipt)
	}
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(controller.state, stateLease)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(leasePath, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := controller.Dispatch(context.Background(), map[string]any{"op": "release", "lease_id": lease})
	if err != nil {
		t.Fatal(err)
	}
	if release["released"] != false || len(release["errors"].([]string)) == 0 {
		t.Fatalf("release falsely claimed durable cleanup: %#v", release)
	}
}

func TestLaunchUsesWolfProfileAndPersistsExactLobbyBeforeJoin(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	result, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err != nil {
		t.Fatal(err)
	}
	if result["lobby_id"] != "created-lobby" || result["runner_container_name"] != "Firefox" {
		t.Fatalf("launch result = %#v", result)
	}
	wolf.mu.Lock()
	if len(wolf.created) != 1 || len(wolf.joined) != 1 {
		wolf.mu.Unlock()
		t.Fatalf("create=%d join=%d", len(wolf.created), len(wolf.joined))
	}
	create := wolf.created[0]
	join := wolf.joined[0]
	wolf.mu.Unlock()
	if create["profile_id"] != "user" || create["runner_state_folder"] != "playtest/user/Firefox" {
		t.Fatalf("create = %#v", create)
	}
	video := create["video_settings"].(map[string]any)
	if video["video_producer_buffer_caps"] != "video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}" {
		t.Fatalf("video buffer caps = %#v", video["video_producer_buffer_caps"])
	}
	if join["lobby_id"] != "created-lobby" || join["moonlight_session_id"] != "our-client" {
		t.Fatalf("join = %#v", join)
	}
	controller.mu.Lock()
	owned := append([]string(nil), controller.lease.Lobbies...)
	controller.mu.Unlock()
	if len(owned) != 1 || owned[0] != "created-lobby" {
		t.Fatalf("owned lobbies = %#v", owned)
	}
	_, err = controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err == nil || !strings.Contains(err.Error(), "already launched") {
		t.Fatalf("duplicate launch error = %v", err)
	}
	wolf.mu.Lock()
	createCount := len(wolf.created)
	wolf.mu.Unlock()
	if createCount != 1 {
		t.Fatalf("duplicate launch created %d lobbies", createCount)
	}
}

func TestLaunchUsesConfiguredSlotRunnerIdentityAndBrowserState(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	if err := controller.SetBrowserRunner("playtest/user/WolfFirefox-slot-2", "WolfFirefox-slot-2"); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err != nil {
		t.Fatal(err)
	}
	if result["runner_container_name"] != "WolfFirefox-slot-2" {
		t.Fatalf("launch result = %#v", result)
	}
	wolf.mu.Lock()
	create := wolf.created[0]
	wolf.mu.Unlock()
	if create["runner_state_folder"] != "playtest/user/WolfFirefox-slot-2" {
		t.Fatalf("runner state = %#v", create["runner_state_folder"])
	}
	runner := create["runner"].(map[string]any)
	if runner["name"] != "WolfFirefox-slot-2" {
		t.Fatalf("runner = %#v", runner)
	}
}

func TestLaunchReplacesBroadInputMountWithLeaseDeviceMounts(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	if err := controller.SetWolfStateDir("/data/services/playtest-wolf"); err != nil {
		t.Fatal(err)
	}
	controller.inputMounts = func(stateDir, clientID string) ([]string, error) {
		if stateDir != "/data/services/playtest-wolf" || clientID != "our-client" {
			t.Fatalf("resolver args %q %q", stateDir, clientID)
		}
		return []string{"/dev/input/event16", "/dev/input/js0"}, nil
	}
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease}); err != nil {
		t.Fatal(err)
	}
	wolf.mu.Lock()
	runner := wolf.created[0]["runner"].(map[string]any)
	mounts := runner["mounts"].([]any)
	wolf.mu.Unlock()
	got, _ := json.Marshal(mounts)
	text := string(got)
	for _, want := range []string{"/tmp/keep:/keep:ro", "/dev/input/event16:/dev/input/event16:ro", "/dev/input/js0:/dev/input/js0:ro"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing isolated mount %q in %s", want, text)
		}
	}
	if strings.Contains(text, "/dev/input:/dev/input:ro") {
		t.Fatalf("broad input mount remains: %s", text)
	}
}

func TestAcquireClearsOnlyItsStaleUIJoystickState(t *testing.T) {
	wolf := &fakeWolf{pressed: make(chan struct{}), pairedClient: "42"}
	stateDir := t.TempDir()
	controller, err := NewController(wolf, t.TempDir(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SetWolfStateDir(stateDir); err != nil {
		t.Fatal(err)
	}
	ownData := filepath.Join(stateDir, "42", "Wolf UI", "udev", "data")
	peerData := filepath.Join(stateDir, "43", "Wolf UI", "udev", "data")
	for _, directory := range []string{ownData, peerData} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	joystick := []byte("E:ID_INPUT=1\nE:ID_INPUT_JOYSTICK=1\n")
	if err := os.WriteFile(filepath.Join(ownData, "c13:80"), joystick, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownData, "c13:81"), []byte("E:ID_INPUT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerData, "c13:80"), joystick, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "acquire"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ownData, "c13:80")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("own stale joystick remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownData, "c13:81")); err != nil {
		t.Fatalf("non-joystick own state removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(peerData, "c13:80")); err != nil {
		t.Fatalf("peer joystick state removed: %v", err)
	}
}

func TestAcquireFailsBeforeLeaseWhenInputStateResetFails(t *testing.T) {
	wolf := &fakeWolf{pressed: make(chan struct{})}
	controller, err := NewController(wolf, t.TempDir(), "our-client")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SetWolfStateDir("/data/services/playtest-wolf"); err != nil {
		t.Fatal(err)
	}
	controller.clearInputState = func(_, _ string) error { return errors.New("state directory unreadable") }
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "acquire"}); err == nil || !strings.Contains(err.Error(), "reset isolated Firefox input readiness") {
		t.Fatalf("acquire error = %v", err)
	}
	controller.mu.Lock()
	lease := controller.lease
	controller.mu.Unlock()
	if lease != nil {
		t.Fatalf("failed acquire persisted lease: %#v", lease)
	}
}

func TestLaunchFailsClosedUntilFreshInputRecordArrives(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	if err := controller.SetWolfStateDir("/data/services/playtest-wolf"); err != nil {
		t.Fatal(err)
	}
	controller.inputMounts = func(_, _ string) ([]string, error) {
		return nil, errors.New("expected exactly one fresh virtual gamepad in state, found 0 after 3s")
	}
	if _, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease}); err == nil || !strings.Contains(err.Error(), "fresh virtual gamepad") {
		t.Fatalf("launch error = %v", err)
	}
	wolf.mu.Lock()
	created := len(wolf.created)
	wolf.mu.Unlock()
	if created != 0 {
		t.Fatalf("launch created Firefox before fresh input was ready: %d", created)
	}
}

func TestFreshJoystickWaitsForCurrentArrivalAndTimesOutClosed(t *testing.T) {
	dataDir := t.TempDir()
	oldTimeout, oldPoll := inputReadyTimeout, inputReadyPoll
	inputReadyTimeout, inputReadyPoll = 100*time.Millisecond, time.Millisecond
	defer func() { inputReadyTimeout, inputReadyPoll = oldTimeout, oldPoll }()
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dataDir, "c13:81"), []byte("E:ID_INPUT_JOYSTICK=1\n"), 0o600)
	}()
	minor, err := waitForFreshJoystickMinor(dataDir)
	if err != nil || minor != 81 {
		t.Fatalf("fresh minor = %d, %v", minor, err)
	}
	if _, err := waitForFreshJoystickMinor(t.TempDir()); err == nil || !strings.Contains(err.Error(), "found 0") {
		t.Fatalf("missing fresh input error = %v", err)
	}
}

func TestJoystickMinorsUsesOnlyVirtualGamepadHWDBEntries(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "c13:80"), []byte("E:ID_INPUT=1\nE:ID_INPUT_JOYSTICK=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "c13:81"), []byte("E:ID_INPUT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	minors, err := joystickMinors(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(minors) != 1 || minors[0] != 80 {
		t.Fatalf("minors = %#v", minors)
	}
}

func TestBrowserRunnerRejectsUnsafeStateFolder(t *testing.T) {
	controller, _, _ := newTarget(t)
	if err := controller.SetBrowserRunner("../shared", "WolfFirefox-slot-2"); err == nil {
		t.Fatal("unsafe runner state folder accepted")
	}
}

func TestLaunchRequiresConfiguredVideoProducerBufferCaps(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	controller.SetVideoProducerBufferCaps("")
	_, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err == nil || !strings.Contains(err.Error(), "--video-producer-buffer-caps") {
		t.Fatalf("launch error = %v", err)
	}
	wolf.mu.Lock()
	createCount := len(wolf.created)
	wolf.mu.Unlock()
	if createCount != 0 {
		t.Fatalf("missing caps created %d lobbies", createCount)
	}
}

func TestLaunchJoinFailureStopsOnlyCreatedLobby(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	controller.mu.Lock()
	controller.lease.Lobbies = []string{"older-owned"}
	if err := controller.saveLocked(); err != nil {
		controller.mu.Unlock()
		t.Fatal(err)
	}
	controller.mu.Unlock()
	wolf.failJoin = true
	_, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err == nil || !strings.Contains(err.Error(), "join created lobby") {
		t.Fatalf("launch error = %v", err)
	}
	wolf.mu.Lock()
	stopped := append([]string(nil), wolf.stopped...)
	wolf.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "created-lobby" {
		t.Fatalf("stopped lobbies = %#v", stopped)
	}
	controller.mu.Lock()
	owned := append([]string(nil), controller.lease.Lobbies...)
	controller.mu.Unlock()
	if len(owned) != 1 || owned[0] != "older-owned" {
		t.Fatalf("failure changed pre-existing ownership: %#v", owned)
	}
}

func TestTimedOutCreateReconcilesAndStopsOnlyExactLaunch(t *testing.T) {
	controller, wolf, lease := newTarget(t)
	wolf.lobbies = append(wolf.lobbies, map[string]any{"id": "unrelated", "name": "Firefox", "connected_sessions": []any{}})
	wolf.failCreate = true
	_, err := controller.Dispatch(context.Background(), map[string]any{"op": "launch", "lease_id": lease})
	if err == nil || !strings.Contains(err.Error(), "reconciled and stopped") {
		t.Fatalf("launch error = %v", err)
	}
	wolf.mu.Lock()
	stopped := append([]string(nil), wolf.stopped...)
	remaining := append([]map[string]any(nil), wolf.lobbies...)
	wolf.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "created-lobby" {
		t.Fatalf("stopped lobbies = %#v", stopped)
	}
	if len(remaining) != 1 || remaining[0]["id"] != "unrelated" {
		t.Fatalf("reconciliation touched unrelated lobby: %#v", remaining)
	}
	controller.mu.Lock()
	intent, owned := controller.lease.FirefoxLaunchName, controller.lease.FirefoxLobbyID
	controller.mu.Unlock()
	if intent != "" || owned != "" {
		t.Fatalf("reconciliation retained launch state name=%q id=%q", intent, owned)
	}
}

func TestManagedBrowserRunnerUsesURLDataAndReplacesStaleEndpoint(t *testing.T) {
	runner := map[string]any{"env": []any{"RUN_GAMESCOPE=1", "PLAYTEST_BROWSER_STARTUP=1", "PLAYTEST_HOST=stale", "PLAYTEST_PORT=1", "PLAYTEST_URL=stale", "KEEP=yes"}}
	compositor, startup, err := configureBrowserRunner(runner, "http://192.168.1.22:37300/path?a=1&b=two#scene")
	if err != nil {
		t.Fatal(err)
	}
	if compositor != "gamescope" || startup != "kiosk" {
		t.Fatalf("%s %s", compositor, startup)
	}
	data, _ := json.Marshal(runner["env"])
	text := string(data)
	for _, expected := range []string{"PLAYTEST_HOST=192.168.1.22", "PLAYTEST_PORT=37300", "PLAYTEST_URL=http://localhost:37300/path?a=1", "KEEP=yes", "MOZ_LEGACY_PROFILES=1"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s in %s", expected, text)
		}
	}
	if strings.Contains(text, "stale") {
		t.Fatal(text)
	}
	for _, invalid := range []string{"", "file:///tmp/game", "http://user:pass@host/", "http://host:0/"} {
		if _, _, err := configureBrowserRunner(map[string]any{"env": []any{"PLAYTEST_BROWSER_STARTUP=1"}}, invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
