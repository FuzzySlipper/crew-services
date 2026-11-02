package board

import (
	"context"
	"errors"
	"net/http"

	"crew-services/internal/fabricclient"
)

// FabricDirectory answers identity questions from the crew messaging fabric's
// public reads. It never writes to the fabric.
type FabricDirectory struct {
	Fabric interface {
		Resolve(ctx context.Context, address string) (fabricclient.Binding, error)
		GetSeat(ctx context.Context, seat string) (fabricclient.Seat, error)
	}
}

func (d FabricDirectory) Session(ctx context.Context, address string) (string, bool, error) {
	binding, err := d.Fabric.Resolve(ctx, address)
	if notFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return binding.AdapterID, binding.Bound, nil
}

func (d FabricDirectory) SeatHolder(ctx context.Context, seat string) (string, bool, error) {
	value, err := d.Fabric.GetSeat(ctx, seat)
	if notFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if value.State == "retired" {
		return "", false, nil
	}
	if value.Holding == nil {
		return "", true, nil
	}
	return value.Holding.HolderAddress, true, nil
}

func notFound(err error) bool {
	var typed *fabricclient.HTTPError
	return errors.As(err, &typed) && typed.Status == http.StatusNotFound
}
