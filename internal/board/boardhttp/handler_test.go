package boardhttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardhttp"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/crewcli"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

// The board commands every driver shares, run against crew-board's HTTP API
// on a real fabric: as a person outside any session, and as a session (its
// address found through a stand-in driver, as `who` finds it).
func TestBoardCommandsEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	persistence, err := sqlite.Open(ctx, filepath.Join(dir, "fabric.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	fsvc, _ := service.New(persistence, service.SystemClock{})
	fabricServer := httptest.NewServer(httpapi.NewHandler(fsvc))
	defer fabricServer.Close()
	fabric, _ := fabricclient.New(fabricServer.URL)
	lease, err := fabric.Register(ctx, "claude-code", "c-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: "claude-code", LeaseToken: lease.LeaseToken, NativeKey: "native-a", Capabilities: []string{"deliver_when_idle"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SeatWrite(ctx, "take", "rusty-engine.lead", fabricclient.SeatWriteRequest{AdapterID: "claude-code", LeaseToken: lease.LeaseToken, OperationID: "take", SessionAddress: binding.Address}); err != nil {
		t.Fatal(err)
	}

	store, err := boardsqlite.Open(ctx, filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := board.New(store, board.FabricDirectory{Fabric: fabric}, time.Now, board.Config{Site: "den", People: []string{"patch", "lore"}})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(boardhttp.NewHandler(svc))
	defer api.Close()
	t.Setenv("CREW_BOARD_URL", api.URL)
	t.Setenv("CREW_BOARD_PERSON", "")
	driver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []crewcli.SessionInfo{{Address: binding.Address, Runtime: "claude-code", NativeID: "native-a", State: "busy"}}})
	}))
	defer driver.Close()
	previous := crewcli.DriverURLs
	crewcli.DriverURLs = map[string]string{"claude-code": driver.URL}
	t.Cleanup(func() { crewcli.DriverURLs = previous })

	run := func(self, stdin string, args ...string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := crewcli.BoardMain(ctx, args, self, strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	if code, _, errOut := run("", "", "new-board", "rusty-engine"); code != 1 || !strings.Contains(errOut, "--person NAME (patch, lore)") {
		t.Fatalf("person-less write = %d %q", code, errOut)
	}
	if code, out, errOut := run("", "", "new-board", "rusty-engine", "--person", "patch", "--note", "Engine design talk"); code != 0 || !strings.Contains(out, "made board rusty-engine") {
		t.Fatalf("new-board = %d %q %q", code, out, errOut)
	}
	body := "Where should the cache index live?\n\n<details><summary>Benchmarks</summary>\na\nb\n</details>\n\ncc @patch @nobody"
	code, out, errOut := run("native-a", body, "new", "rusty-engine", "Shader cache", "--as", "rusty-engine.lead")
	if code != 0 || !strings.HasPrefix(out, "opened thread den-") || !strings.Contains(errOut, "@nobody stays plain text") {
		t.Fatalf("new = %d %q %q", code, out, errOut)
	}
	thread := strings.Fields(out)[2]
	if code, out, _ := run("", "", "reply", thread, "--person", "lore", "Per material, I think."); code != 0 || !strings.Contains(out, "posted den-") {
		t.Fatalf("reply = %d %q", code, out)
	}
	// lore answered the seat's post, so the reply is unread for the seat
	// until it reads; a --peek read moves nothing.
	if code, out, _ := run("native-a", "", "unread", "--as", "rusty-engine.lead"); code != 0 || !strings.Contains(out, thread) || !strings.Contains(out, "Shader cache") {
		t.Fatalf("unread = %d %q", code, out)
	}
	run("native-a", "", "read", thread, "--as", "rusty-engine.lead", "--peek")
	if _, out, _ := run("native-a", "", "unread", "--as", "rusty-engine.lead"); !strings.Contains(out, thread) {
		t.Fatalf("unread after a peek = %q", out)
	}
	run("native-a", "", "read", thread, "--as", "rusty-engine.lead")
	if _, out, _ := run("native-a", "", "unread", "--as", "rusty-engine.lead"); !strings.Contains(out, "Nothing unread.") {
		t.Fatalf("unread after reading = %q", out)
	}
	if code, out, _ := run("", "", "notices"); code != 0 || !strings.Contains(out, "rusty-engine.lead  reply") {
		t.Fatalf("notices = %d %q", code, out)
	}
	code, out, _ = run("native-a", "", "read", thread)
	if code != 0 || !strings.Contains(out, "Shader cache\n"+thread+" · board rusty-engine · open · 2 posts") ||
		!strings.Contains(out, "rusty-engine.lead (Claude Code, "+binding.Address+")") ||
		!strings.Contains(out, "[folded: Benchmarks, 2 lines; --unfold to show]") || !strings.Contains(out, "· lore · reply to den-") {
		t.Fatalf("read = %d\n%s", code, out)
	}
	if _, out, _ := run("native-a", "", "read", thread, "--unfold"); !strings.Contains(out, "[Benchmarks]\na\nb\n[end of Benchmarks]") {
		t.Fatalf("unfolded read:\n%s", out)
	}
	if code, _, errOut := run("native-a", "", "reply", thread, "--as", "rusty-engine.other", "x"); code != 1 || !strings.Contains(errOut, "not_holder") {
		t.Fatalf("reply as a seat not held = %d %q", code, errOut)
	}
	if code, out, _ := run("", "", "threads", "rusty-engine"); code != 0 || !strings.Contains(out, thread) || !strings.Contains(out, "Shader cache") {
		t.Fatalf("threads = %d %q", code, out)
	}
	if code, out, _ := run("", "", "search", "rusty-engine", "per material"); code != 0 || !strings.Contains(out, "Per material, I think.") {
		t.Fatalf("search = %d %q", code, out)
	}
	var read board.ThreadRead
	_, out, _ = run("", "", "read", thread, "--json")
	if err := json.Unmarshal([]byte(out), &read); err != nil || len(read.Posts) != 2 {
		t.Fatalf("read --json = %q, %v", out, err)
	}
	opening := read.Posts[0].ID
	if code, out, _ := run("native-a", "", "edit", opening, "--as", "rusty-engine.lead", "Where should the cache index live? (edited)"); code != 0 || !strings.Contains(out, "now revision 2") {
		t.Fatalf("edit = %d %q", code, out)
	}
	if code, _, errOut := run("", "", "edit", opening, "--person", "lore", "hijack"); code != 1 || !strings.Contains(errOut, "not_author") {
		t.Fatalf("edit by someone else = %d %q", code, errOut)
	}
	if code, out, _ := run("", "", "revisions", opening); code != 0 || !strings.Contains(out, "revision 1") || !strings.Contains(out, "(edited)") {
		t.Fatalf("revisions = %d %q", code, out)
	}
	reply := read.Posts[1].ID
	if code, out, _ := run("", "", "retract", reply, "--person", "lore", "--note", "spoke too soon"); code != 0 || !strings.Contains(out, "retracted") {
		t.Fatalf("retract = %d %q", code, out)
	}
	if _, out, _ := run("", "", "read", reply); !strings.Contains(out, "[retracted by its author: spoke too soon]") {
		t.Fatalf("read retracted post:\n%s", out)
	}
	if code, _, errOut := run("native-a", "", "hide", opening); code != 1 || !strings.Contains(errOut, "person_only") {
		t.Fatalf("hide from a session = %d %q", code, errOut)
	}
}
