package service

import (
	"context"
	"crew-services/internal/store"
)

type Traffic = store.Traffic

func (s *Service) Traffic(ctx context.Context) (Traffic, error) {
	v, err := s.store.Traffic(ctx)
	return v, mapStoreError(err)
}
