package codexadapter

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

// TestLiveCurrentCodexAppServerReadProjection is intentionally opt-in. It
// starts the currently installed local App Server, reads an existing thread,
// and writes only to a scratch fabric database; it never mutates Codex.
func TestLiveCurrentCodexAppServerReadProjection(t *testing.T) {
	if os.Getenv("CREW_CODEX_LIVE") != "1" {
		t.Skip("set CREW_CODEX_LIVE=1 to exercise the installed Codex App Server")
	}
	native, err := StartStdioAppServer("codex", []string{"app-server", "--stdio"})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := native.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	threads, err := native.ListThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) == 0 {
		t.Skip("the current Codex home has no existing threads to read")
	}
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "crew-codex-live.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	svc, err := service.New(persistence, service.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewHandler(svc))
	defer server.Close()
	fabric, err := NewHTTPFabric(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fabric.Register(ctx, "crew-codex-live", "crew-codex-live", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	projector := Projector{Fabric: fabric, Native: native, AdapterID: lease.AdapterID, Lease: lease, Mappings: []Mapping{{Address: "crew/live", ThreadID: threads[0].ID}}, Capabilities: []string{"session-events", "read-only-native-history"}}
	if err := projector.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	binding, err := fabric.Resolve(ctx, "crew/live")
	if err != nil {
		t.Fatal(err)
	}
	if !binding.Bound || binding.TargetRef == "" || binding.TargetRef == threads[0].ID {
		t.Fatalf("expected public fabric session target, got %+v", binding)
	}
}
