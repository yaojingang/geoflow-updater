package operation

import (
	"context"
	"errors"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"syscall"
	"time"
)

const KindSwitchBack Kind = "switch-back"

func (manager *Manager) Preview(ctx context.Context, id string) (update.PlanSummary, error) {
	timeout := 25 * time.Minute
	if manager.PreviewTimeout > 0 && manager.PreviewTimeout < timeout {
		timeout = manager.PreviewTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !instanceIDPattern.MatchString(id) {
		return update.PlanSummary{}, errors.New("invalid instance")
	}
	manager.mu.Lock()
	if manager.active[id] != "" {
		manager.mu.Unlock()
		return update.PlanSummary{}, ErrActive
	}
	lock, err := manager.acquireLock(id)
	manager.mu.Unlock()
	if err != nil {
		return update.PlanSummary{}, err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
	previewer, ok := manager.Deployment.(interface {
		Preview(context.Context, string) (update.PlanSummary, error)
	})
	if !ok {
		return update.PlanSummary{}, errors.New("upgrade planning is unavailable")
	}
	return previewer.Preview(ctx, id)
}

func (manager *Manager) StartSwitchBack(id string) (Operation, error) {
	return manager.start(id, KindSwitchBack, "", func(ctx context.Context, operation *Operation, save func() error) {
		deployment, ok := manager.Deployment.(interface {
			SwitchBack(context.Context, string, string, update.Observer) update.Result
		})
		if !ok {
			operation.Status = StatusFailed
			operation.Error = "application switch-back is unavailable"
			return
		}
		result := deployment.SwitchBack(ctx, id, operation.ID, func(stage update.Stage) error { return manager.updateStage(operation, stage, save) })
		operation.Error = result.Error
		operation.TargetVersion = result.Target.Version
		switch result.Status {
		case update.StatusSucceeded, update.StatusRolledBack:
			operation.Status = StatusSucceeded
		case update.StatusRecoveryRequired:
			operation.Status = StatusRecoveryRequired
		default:
			operation.Status = StatusFailed
		}
	})
}
