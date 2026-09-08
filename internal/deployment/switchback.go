package deployment

import (
	"context"
	"errors"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"time"
)

// SwitchBack only reactivates a compatible retained application. Business data stays live.
func (service *Service) SwitchBack(ctx context.Context, id, operationID string, observe update.Observer) update.Result {
	fail := func(err error) update.Result { return update.Result{Status: update.StatusFailed, Error: err.Error()} }
	if !safeOperationID(operationID) {
		return fail(errors.New("invalid operation identity"))
	}
	tx, err := service.readTransaction(id)
	if err != nil {
		return fail(err)
	}
	if tx.Strategy != managed.StrategyOnline || tx.Source.Layout != LayoutBlueGreen || tx.Status != update.StatusSucceeded {
		return fail(errors.New("only a completed compatible online release has an application switch-back"))
	}
	config, err := service.loadConfig(id)
	if err != nil {
		return fail(err)
	}
	if config.ReleaseSequence != tx.Target.Sequence || config.ActiveSlot != tx.Candidate.ActiveSlot {
		return fail(errors.New("retained release no longer matches current deployment"))
	}
	tx.OperationID = operationID
	tx.Stage = "switch-back"
	tx.Status = "running"
	if err := service.saveTransaction(&tx); err != nil {
		return fail(err)
	}
	if observe != nil {
		if err := observe(update.Stage{Name: "switch-back", Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
			return service.failRelease(ctx, &tx, err, nil)
		}
	}
	if err := service.restoreApplication(ctx, &tx); err != nil {
		tx.Status = update.StatusRecoveryRequired
		tx.Error = err.Error()
		_ = service.saveTransaction(&tx)
		return update.Result{Status: tx.Status, Error: tx.Error}
	}
	tx.Status = update.StatusRolledBack
	tx.Error = ""
	if err := service.saveTransaction(&tx); err != nil {
		return update.Result{Status: update.StatusRecoveryRequired, Error: err.Error()}
	}
	if observe != nil {
		if err := observe(update.Stage{Name: "switch-back", Status: "succeeded", UpdatedAt: time.Now().UTC()}); err != nil {
			return update.Result{Status: update.StatusRecoveryRequired, Error: err.Error()}
		}
	}
	return update.Result{Status: update.StatusSucceeded, Target: managed.Release{Version: tx.Source.Version, Sequence: tx.Source.ReleaseSequence}}
}
