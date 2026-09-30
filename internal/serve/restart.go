package serve

import (
	"context"
	"errors"

	"crew-services/internal/devserver"
)

type RestartManager interface {
	Status(context.Context, devserver.StatusOptions) (devserver.SessionState, error)
	Stop(context.Context, devserver.StopOptions) (devserver.StopResult, error)
	Up(context.Context, devserver.UpOptions) (devserver.UpResult, error)
}

type RestartService struct {
	manager RestartManager
}

func NewRestartService(manager RestartManager) *RestartService {
	return &RestartService{manager: manager}
}

func (s *RestartService) Restart(ctx context.Context, options devserver.UpOptions) (devserver.UpResult, error) {
	session, err := s.manager.Status(ctx, devserver.StatusOptions{
		Project:  options.Project,
		RepoRoot: options.RepoRoot,
	})
	switch {
	case errors.Is(err, devserver.ErrSessionNotFound):
		// Nothing is up yet, so restart has the same result as up.
	case err != nil:
		return devserver.UpResult{}, err
	case session.Ownership != "broker_owned" && session.Status != "stopped":
		return devserver.UpResult{}, errors.New("session is not broker-owned; refusing to restart an external process")
	case session.Status != "stopped":
		if _, err := s.manager.Stop(ctx, devserver.StopOptions{
			Project:  options.Project,
			RepoRoot: options.RepoRoot,
		}); err != nil {
			return devserver.UpResult{}, err
		}
	}
	return s.manager.Up(ctx, options)
}
