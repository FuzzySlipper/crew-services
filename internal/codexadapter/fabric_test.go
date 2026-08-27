package codexadapter

import (
	"context"
	"net/http/httptest"
	"path/filepath"
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
