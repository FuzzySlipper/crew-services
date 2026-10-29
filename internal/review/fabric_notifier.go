package review

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"crew-services/internal/fabricclient"
)

const (
	// FabricProducerID is crew-review's fabric adapter ID. The fabric only
	// accepts a sender address bound to the submitting producer, so crew-review
	// holds a lease under this ID and binds its own origin address to it.
	FabricProducerID = "crew-review"
	// FabricOriginAddress is crew-review's service-owned origin. It is not an
	// agent address: nothing drives it, and a reply to it is never delivered.
	FabricOriginAddress = "system/crew-review"
)

// FabricClient is the subset of the crew-messaging client the notifier uses.
type FabricClient interface {
	Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (fabricclient.Lease, error)
	Renew(ctx context.Context, adapterID, token string, duration time.Duration) (fabricclient.Lease, error)
	Resolve(ctx context.Context, address string) (fabricclient.Binding, error)
	Bind(ctx context.Context, address string, request fabricclient.BindRequest) (fabricclient.Binding, error)
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
}

// FabricNotifierConfig names crew-review's producer identity.
type FabricNotifierConfig struct {
	AdapterID string
	// InstanceID is stable across restarts so a restarted crew-review takes
	// over its own lease instead of waiting for the old one to expire.
	InstanceID    string
	Address       string
	LeaseDuration time.Duration
}

// FabricNotifier submits outbox notices to the crew messaging fabric as the
// producer system/crew-review. It registers lazily, so an unavailable fabric
// only delays notices.
type FabricNotifier struct {
	client FabricClient
	clock  Clock
	cfg    FabricNotifierConfig

	mu    sync.Mutex
	lease fabricclient.Lease
	bound bool
}

func NewFabricNotifier(client FabricClient, clock Clock, cfg FabricNotifierConfig) (*FabricNotifier, error) {
	if client == nil {
		return nil, errors.New("fabric client is required")
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if cfg.AdapterID == "" {
		cfg.AdapterID = FabricProducerID
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = cfg.AdapterID
	}
	if cfg.Address == "" {
		cfg.Address = FabricOriginAddress
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	return &FabricNotifier{client: client, clock: clock, cfg: cfg}, nil
}

func (n *FabricNotifier) Notify(ctx context.Context, notice Notice) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.ensureLease(ctx); err != nil {
		return "", err
	}
	if err := n.ensureOrigin(ctx); err != nil {
		return "", err
	}
	result, err := n.client.Submit(ctx, fabricclient.MessageRequest{
		ProducerID:       n.cfg.AdapterID,
		LeaseToken:       n.lease.LeaseToken,
		OperationID:      notice.OperationID,
		SenderAddress:    n.cfg.Address,
		RecipientAddress: notice.Recipient,
		Body:             notice.Body,
		ActivationPolicy: NoticeActivationPolicy,
		TTL:              NoticeTTL.String(),
	})
	if err == nil {
		return result.Message.MessageID, nil
	}
	switch fabricclient.Code(err) {
	case "lease_expired", "lease_fenced":
		n.lease = fabricclient.Lease{}
		n.bound = false
	case "adapter_mismatch":
		// The origin was rebound away from crew-review; check it next time.
		n.bound = false
	case "invalid_request", "operation_conflict":
		return "", fmt.Errorf("%w: %v", ErrNoticeRejected, err)
	case "not_found", "not_bound":
		// The recipient has no live binding. The session may register its
		// address later, so the notice is retried with backoff until it
		// expires after NoticeTTL, without holding back other notices.
		// The origin binding is re-checked on the next call, so a lost
		// origin shows up there as a fabric failure instead of being
		// mistaken for a bad recipient on every notice.
		n.bound = false
		return "", fmt.Errorf("%w: %s: %v", ErrRecipientUnavailable, notice.Recipient, err)
	}
	return "", err
}

func (n *FabricNotifier) ensureLease(ctx context.Context) error {
	now := n.clock.Now()
	if n.lease.LeaseToken != "" && now.Add(n.cfg.LeaseDuration/2).Before(n.lease.ExpiresAt) {
		return nil
	}
	if n.lease.LeaseToken != "" {
		lease, err := n.client.Renew(ctx, n.cfg.AdapterID, n.lease.LeaseToken, n.cfg.LeaseDuration)
		if err == nil {
			n.lease = lease
			return nil
		}
		if code := fabricclient.Code(err); code != "lease_expired" && code != "lease_fenced" {
			return fmt.Errorf("renew fabric lease: %w", err)
		}
	}
	lease, err := n.client.Register(ctx, n.cfg.AdapterID, n.cfg.InstanceID, n.cfg.LeaseDuration)
	if err != nil {
		n.lease = fabricclient.Lease{}
		return fmt.Errorf("register with fabric: %w", err)
	}
	n.lease = lease
	return nil
}

// ensureOrigin binds the origin address to crew-review. The binding names
// crew-review itself as the target; no driver claims deliveries for it.
func (n *FabricNotifier) ensureOrigin(ctx context.Context) error {
	if n.bound {
		return nil
	}
	request := fabricclient.BindRequest{ActorAdapterID: n.cfg.AdapterID, LeaseToken: n.lease.LeaseToken, AdapterID: n.cfg.AdapterID, TargetRef: n.cfg.AdapterID, Capabilities: []string{}}
	binding, err := n.client.Resolve(ctx, n.cfg.Address)
	switch {
	case err == nil && binding.Bound && binding.AdapterID == n.cfg.AdapterID:
		n.bound = true
		return nil
	case err == nil && binding.Bound:
		return fmt.Errorf("fabric address %s is bound to adapter %s, not %s", n.cfg.Address, binding.AdapterID, n.cfg.AdapterID)
	case err == nil:
		revision := binding.Revision
		request.ExpectedRevision = &revision
	case !isFabricNotFound(err):
		return fmt.Errorf("resolve fabric origin %s: %w", n.cfg.Address, err)
	}
	if _, err := n.client.Bind(ctx, n.cfg.Address, request); err != nil {
		return fmt.Errorf("bind fabric origin %s: %w", n.cfg.Address, err)
	}
	n.bound = true
	return nil
}

func isFabricNotFound(err error) bool {
	var httpErr *fabricclient.HTTPError
	return fabricclient.Code(err) == "not_found" || errors.As(err, &httpErr) && httpErr.Status == 404
}
