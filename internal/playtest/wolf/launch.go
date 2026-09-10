package wolf

import (
	"context"
	"errors"
)

// LaunchFirefox asks the target to create and join its configured Firefox
// lobby to this adapter's Moonlight session. The target resolves the machine
// profile and owns the resulting lobby ID; callers cannot supply a runner.
func (a *Adapter) LaunchFirefox(ctx context.Context, leaseID, gameURL string) (map[string]any, error) {
	a.mu.Lock()
	if err := a.requireLocked(leaseID); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if a.closing {
		a.mu.Unlock()
		return nil, errors.New("Wolf lease is closing")
	}
	directory := a.directory
	a.mu.Unlock()
	result, err := a.remote(ctx, "launch", map[string]any{"lease_id": leaseID, "url": gameURL})
	if err != nil {
		return nil, err
	}
	if err := a.journalAt(directory, mergeEvent("firefox_launch", result)); err != nil {
		return nil, err
	}
	return result, nil
}
