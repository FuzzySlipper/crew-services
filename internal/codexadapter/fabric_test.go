package codexadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

func TestHTTPFabricPreservesEscapedAddressSegment(t *testing.T) {
	ctx := context.Background()
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fabric.sqlite"))
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
	lease, err := fabric.Register(ctx, "crew-codex", "test-instance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"codex/dsh-crew", "codex/dsh crew"} {
		t.Run(address, func(t *testing.T) {
			bound, err := fabric.Bind(ctx, address, BindRequest{
				ActorAdapterID: lease.AdapterID,
				LeaseToken:     lease.LeaseToken,
				AdapterID:      lease.AdapterID,
				TargetRef:      "codex-thread-" + address,
			})
			if err != nil {
				t.Fatal(err)
			}
			if bound.Address != address {
				t.Fatalf("bound address = %q, want %q", bound.Address, address)
			}

			resolved, err := fabric.Resolve(ctx, address)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Address != address {
				t.Fatalf("resolved address = %q, want %q", resolved.Address, address)
			}
		})
	}
}

func TestHTTPFabricReusesItsConnectionAfterLargeResponses(t *testing.T) {
	ctx := context.Background()
	// A listing larger than the server's buffer is sent chunked; the decoder
	// stops before the encoder's trailing newline and the final chunk.
	deliveries := make([]Delivery, 400)
	for i := range deliveries {
		deliveries[i] = Delivery{DeliveryID: fmt.Sprintf("delivery-%d", i), RecipientAddress: "codex/reviewer", State: "delivered"}
	}
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/deliveries":
			_ = json.NewEncoder(w).Encode(map[string]any{"deliveries": deliveries})
		case "/v1/addresses/codex%2Fmissing", "/v1/addresses/codex/missing":
			http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	fabric, err := NewHTTPFabric(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		listed, err := fabric.Deliveries(ctx)
		if err != nil || len(listed) != len(deliveries) {
			t.Fatalf("listed=%d err=%v", len(listed), err)
		}
		if err := fabric.Append(ctx, "session", AppendRequest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := fabric.Resolve(ctx, "codex/missing"); err == nil {
			t.Fatal("missing address resolved")
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("fabric opened %d connections for sequential calls, want 1", got)
	}
}

func TestProjectorPassOverUnchangedThreadWritesNothingToTheFabric(t *testing.T) {
	ctx := context.Background()
	persistence, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fabric.sqlite"))
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
	lease, err := fabric.Register(ctx, "crew-codex", "test-instance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	native := &fakeAppServer{threads: []NativeThread{thread("thread-1", "Scout", "idle", []NativeTurn{{ID: "turn-1", Status: "completed", Items: []NativeItem{
		{ID: "user-1", Type: "userMessage", Content: []NativeContent{{Type: "text", Text: "survey this"}}},
	}}})}}
	revisions := func() (int64, int64) {
		t.Helper()
		binding, err := fabric.Resolve(ctx, "crew/scout")
		if err != nil {
			t.Fatal(err)
		}
		session, err := fabric.Adopt(ctx, AdoptRequest{AdapterID: lease.AdapterID, LeaseToken: lease.LeaseToken, AdapterKey: "thread-1", Label: "Scout", Status: "idle"})
		if err != nil {
			t.Fatal(err)
		}
		return binding.Revision, session.Revision
	}
	// codexCapabilities is deliberately unsorted; the fabric stores it sorted.
	projector := Projector{Fabric: fabric, Native: native, AdapterID: lease.AdapterID, Lease: lease, Mappings: []Mapping{{Address: "crew/scout", ThreadID: "thread-1"}}, Capabilities: codexCapabilities, ClaimDuration: time.Minute}
	if err := projector.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	bindingRevision, sessionRevision := revisions()
	for range 3 {
		if err := projector.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if gotBinding, gotSession := revisions(); gotBinding != bindingRevision || gotSession != sessionRevision {
		t.Fatalf("unchanged passes rewrote binding %d->%d, session %d->%d", bindingRevision, gotBinding, sessionRevision, gotSession)
	}
}
