package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
)

// SessionAddressPrefix marks fabric-generated per-session addresses.
const SessionAddressPrefix = "s/"

// RegisterSessionAddressRequest asks for the address of one native session an
// adapter drives. NativeKey is the adapter's opaque name for that session; it
// becomes the binding's target_ref and never appears in the address itself.
type RegisterSessionAddressRequest struct {
	AdapterID    string
	LeaseToken   string
	NativeKey    string
	Capabilities []string
}

// SessionAddress derives the stable address for (adapterID, nativeKey). The
// same native session always gets the same address, including after a resume
// or a fabric restart, without any extra table.
func SessionAddress(adapterID, nativeKey string) string {
	sum := sha256.Sum256([]byte(adapterID + "\x00" + nativeKey))
	return SessionAddressPrefix + hex.EncodeToString(sum[:8])
}

// RegisterSessionAddress binds the session's address to the calling adapter,
// creating it on first use. Registering again with the same capabilities
// returns the binding unchanged; changed capabilities advance its revision but
// not its generation, so queued work stays deliverable.
func (s *Service) RegisterSessionAddress(ctx context.Context, request RegisterSessionAddressRequest) (Binding, error) {
	for _, field := range []struct{ name, value string }{{"adapter_id", request.AdapterID}, {"lease_token", request.LeaseToken}, {"native_key", request.NativeKey}} {
		if err := required(field.name, field.value); err != nil {
			return Binding{}, err
		}
	}
	capabilities, err := canonicalCapabilities(request.Capabilities)
	if err != nil {
		return Binding{}, err
	}
	address := SessionAddress(request.AdapterID, request.NativeKey)
	put := PutBindingRequest{Address: address, ActorAdapterID: request.AdapterID, LeaseToken: request.LeaseToken, AdapterID: request.AdapterID, TargetRef: request.NativeKey, Capabilities: capabilities}
	// Two registrations of one session can race; the loser re-reads.
	for attempt := 0; attempt < 3; attempt++ {
		existing, err := s.Resolve(ctx, address)
		var typed *Error
		if errors.As(err, &typed) && typed.Code == CodeNotFound {
			binding, err := s.PutBinding(ctx, put)
			if hasCode(err, CodeConflict) || hasCode(err, CodeStaleRevision) {
				continue
			}
			return binding, err
		}
		if err != nil {
			return Binding{}, err
		}
		if existing.Bound && existing.AdapterID != request.AdapterID {
			return Binding{}, &Error{Code: CodeAdapterMismatch, Err: errors.New("session address is bound to another adapter")}
		}
		if existing.Bound && existing.TargetRef == request.NativeKey && slices.Equal(existing.Capabilities, capabilities) {
			return existing, nil
		}
		revision := existing.Revision
		put.ExpectedRevision = &revision
		binding, err := s.PutBinding(ctx, put)
		if hasCode(err, CodeStaleRevision) {
			continue
		}
		return binding, err
	}
	return Binding{}, &Error{Code: CodeConflict, Err: errors.New("session address registration kept racing")}
}

// PublicBinding is the client-visible view of a binding. A session address's
// target_ref is the adapter's native session key; it stays adapter-private, so
// it is dropped here. The owning adapter keeps its own native mapping and never
// needs it back. A binding an adapter put with an explicit target_ref (a pinned
// role address) keeps it: the adapter chose that value through the public API.
func PublicBinding(binding Binding) Binding {
	if strings.HasPrefix(binding.Address, SessionAddressPrefix) {
		binding.TargetRef = ""
	}
	return binding
}

// PublicBindings applies PublicBinding to a directory listing.
func PublicBindings(bindings []Binding) []Binding {
	out := make([]Binding, len(bindings))
	for i, binding := range bindings {
		out[i] = PublicBinding(binding)
	}
	return out
}

func hasCode(err error, code ErrorCode) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}
