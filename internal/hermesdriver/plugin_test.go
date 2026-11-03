package hermesdriver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/dshdriver"
)

// The crew plugin, loaded by Python from kate's plugins/ as Hermes would,
// tells each new chat its intro once and attaches spooled mail exactly once.
func TestThePluginAttachesSpooledMailOnceAndIntroducesEachChat(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	h, root := install(t)
	if _, err := h.Install(); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteIntro("kate", "Your crew messaging address is s/kate."); err != nil {
		t.Fatal(err)
	}
	h.now = time.Now
	for _, text := range []string{"first notice", "second notice"} {
		if err := h.Inject(context.Background(), dshdriver.PromptRequest{SessionID: "kate", RequestID: "r-" + text, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("crew", sys.argv[1])
mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
hooks = {}
class Ctx:
    def register_hook(self, name, fn): hooks[name] = fn
mod.register(Ctx())
call = hooks["pre_llm_call"]
out = [call(session_id="chat-1", is_first_turn=True), call(session_id="chat-1"), call(session_id="chat-2")]
print(json.dumps({"profile": mod.INSTALLED, "out": out}))
`
	cmd := exec.Command("python3", "-I", "-c", script, filepath.Join(root, "profiles", "kate", "plugins", "crew", "__init__.py"))
	cmd.Env = append(os.Environ(), "CREW_HERMES_STATE="+h.Dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{`"profile": "kate"`, `{"context": "Your crew messaging address is s/kate.\n\nfirst notice\n\nsecond notice"}, null, {"context": "Your crew messaging address is s/kate."}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("plugin output lacks %s:\n%s", want, got)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(h.Dir, "pending", "kate", "*.json")); len(left) != 0 {
		t.Fatalf("spooled mail left after it was attached: %v", left)
	}
}

// A spool file crew-hermes has written but not yet renamed into place is not
// published: the plugin attaches nothing until it is, then attaches it once.
func TestThePluginIgnoresASpoolFileStillBeingWritten(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	h, root := install(t)
	if _, err := h.Install(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.Dir, "pending", "kate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, ".1-r1.json")
	if err := os.WriteFile(temp, []byte(`{"request_id":"r1","text":"half-published notice"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("crew", sys.argv[1])
mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
hooks = {}
class Ctx:
    def register_hook(self, name, fn): hooks[name] = fn
mod.register(Ctx())
print(json.dumps(hooks["pre_llm_call"](session_id=sys.argv[2])))
`
	run := func(session string) string {
		cmd := exec.Command("python3", "-I", "-c", script, filepath.Join(root, "profiles", "kate", "plugins", "crew", "__init__.py"), session)
		cmd.Env = append(os.Environ(), "CREW_HERMES_STATE="+h.Dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("plugin: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run("chat-1"); got != "null" {
		t.Fatalf("an unpublished file was attached: %s", got)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("the unpublished file was moved: %v", err)
	}
	if err := os.Rename(temp, filepath.Join(dir, "1-r1.json")); err != nil {
		t.Fatal(err)
	}
	if got := run("chat-1"); !strings.Contains(got, "half-published notice") {
		t.Fatalf("the published file was not attached: %s", got)
	}
	if got := run("chat-1"); got != "null" {
		t.Fatalf("attached twice: %s", got)
	}
}
