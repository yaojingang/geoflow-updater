package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type Status string

const (
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
)

type Stage struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Result struct {
	Status          Status          `json:"status"`
	Target          managed.Release `json:"-"`
	RecoveryPointID string          `json:"recovery_point_id,omitempty"`
	Error           string          `json:"error,omitempty"`
}

type Deployment interface {
	Resolve(context.Context, string) (managed.Release, error)
	Preflight(context.Context, string, managed.Release) error
	Pull(context.Context, string, managed.Release) error
	Quiesce(context.Context, string) error
	CreateRecoveryPoint(context.Context, string, string) (string, error)
	Migrate(context.Context, string, managed.Release) error
	Activate(context.Context, string, managed.Release) error
	Rollback(context.Context, string, string) error
	Resume(context.Context, string) error
	Verify(context.Context, string) error
}

type Engine struct {
	Deployment      Deployment
	Now             func() time.Time
	RecoveryTimeout time.Duration
}

type Observer func(Stage) error

func (engine Engine) Run(ctx context.Context, instanceID string, observe Observer) Result {
	if engine.Deployment == nil {
		return Result{Status: StatusFailed, Error: "deployment service is unavailable"}
	}
	emit := func(name string, status string, message string) error {
		if observe != nil {
			return observe(Stage{Name: name, Status: status, Message: message, UpdatedAt: engine.now().UTC()})
		}
		return nil
	}
	fail := func(stage string, err error) Result {
		if observeErr := emit(stage, "failed", err.Error()); observeErr != nil {
			err = errors.Join(err, fmt.Errorf("persist %s failure: %w", stage, observeErr))
		}
		return Result{Status: StatusFailed, Error: err.Error()}
	}
	persistenceFailure := func(stage string, err error) Result {
		return fail(stage, fmt.Errorf("persist operation state: %w", err))
	}

	if err := emit("resolve", "running", ""); err != nil {
		return persistenceFailure("resolve", err)
	}
	target, err := engine.Deployment.Resolve(ctx, instanceID)
	if err != nil {
		return fail("resolve", err)
	}
	if err := emit("resolve", "succeeded", ""); err != nil {
		return persistenceFailure("resolve", err)
	}

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"preflight", func() error { return engine.Deployment.Preflight(ctx, instanceID, target) }},
		{"pull", func() error { return engine.Deployment.Pull(ctx, instanceID, target) }},
	} {
		if err := emit(step.name, "running", ""); err != nil {
			return persistenceFailure(step.name, err)
		}
		if err := step.run(); err != nil {
			return fail(step.name, err)
		}
		if err := emit(step.name, "succeeded", ""); err != nil {
			return persistenceFailure(step.name, err)
		}
	}

	if err := emit("quiesce", "running", ""); err != nil {
		return persistenceFailure("quiesce", err)
	}
	if err := engine.Deployment.Quiesce(ctx, instanceID); err != nil {
		recoveryCtx, cancel := engine.recoveryContext(ctx)
		defer cancel()
		if resumeErr := engine.Deployment.Resume(recoveryCtx, instanceID); resumeErr != nil {
			err = errors.Join(err, fmt.Errorf("resume after failed quiesce: %w", resumeErr))
		} else if verifyErr := engine.Deployment.Verify(recoveryCtx, instanceID); verifyErr != nil {
			err = errors.Join(err, fmt.Errorf("verify after failed quiesce: %w", verifyErr))
		}
		return fail("quiesce", err)
	}
	if err := emit("quiesce", "succeeded", ""); err != nil {
		return engine.resumeAfterPersistenceFailure(ctx, instanceID, target, "quiesce", err, emit)
	}

	if err := emit("backup", "running", ""); err != nil {
		return engine.resumeAfterPersistenceFailure(ctx, instanceID, target, "backup", err, emit)
	}
	recoveryPointID, err := engine.Deployment.CreateRecoveryPoint(ctx, instanceID, "update-to-"+target.Version)
	if err != nil {
		recoveryCtx, cancel := engine.recoveryContext(ctx)
		defer cancel()
		resumeErr := engine.Deployment.Resume(recoveryCtx, instanceID)
		if resumeErr != nil {
			err = errors.Join(err, fmt.Errorf("resume current release: %w", resumeErr))
		}
		return fail("backup", err)
	}
	if err := emit("backup", "succeeded", recoveryPointID); err != nil {
		return engine.rollback(ctx, instanceID, recoveryPointID, target, fmt.Errorf("persist backup recovery point: %w", err), emit)
	}

	protectedSteps := []struct {
		name string
		run  func() error
	}{
		{"migrate", func() error { return engine.Deployment.Migrate(ctx, instanceID, target) }},
		{"activate", func() error { return engine.Deployment.Activate(ctx, instanceID, target) }},
		{"resume", func() error { return engine.Deployment.Resume(ctx, instanceID) }},
		{"verify", func() error { return engine.Deployment.Verify(ctx, instanceID) }},
	}
	for _, step := range protectedSteps {
		if err := emit(step.name, "running", ""); err != nil {
			return engine.rollback(ctx, instanceID, recoveryPointID, target, fmt.Errorf("persist %s stage: %w", step.name, err), emit)
		}
		if err := step.run(); err != nil {
			if observeErr := emit(step.name, "failed", err.Error()); observeErr != nil {
				err = errors.Join(err, fmt.Errorf("persist %s failure: %w", step.name, observeErr))
			}
			return engine.rollback(ctx, instanceID, recoveryPointID, target, err, emit)
		}
		if err := emit(step.name, "succeeded", ""); err != nil {
			return engine.rollback(ctx, instanceID, recoveryPointID, target, fmt.Errorf("persist %s completion: %w", step.name, err), emit)
		}
	}

	if err := emit("succeeded", "succeeded", ""); err != nil {
		return engine.rollback(ctx, instanceID, recoveryPointID, target, fmt.Errorf("persist update completion: %w", err), emit)
	}
	return Result{Status: StatusSucceeded, Target: target, RecoveryPointID: recoveryPointID}
}

func (engine Engine) rollback(
	ctx context.Context,
	instanceID string,
	recoveryPointID string,
	target managed.Release,
	cause error,
	emit func(string, string, string) error,
) Result {
	recoveryCtx, cancel := engine.recoveryContext(ctx)
	defer cancel()

	observeErr := emit("rollback", "running", "")
	if err := engine.Deployment.Quiesce(recoveryCtx, instanceID); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("quiesce before automatic rollback: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := engine.Deployment.Rollback(recoveryCtx, instanceID, recoveryPointID); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("automatic rollback: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := engine.Deployment.Resume(recoveryCtx, instanceID); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("resume rolled back release: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := engine.Deployment.Verify(recoveryCtx, instanceID); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("verify rolled back release: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	observeErr = errors.Join(observeErr, emit("rolled_back", "succeeded", cause.Error()))
	if observeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("persist rollback state: %w", observeErr))
	}

	return Result{Status: StatusRolledBack, Target: target, RecoveryPointID: recoveryPointID, Error: cause.Error()}
}

func (engine Engine) resumeAfterPersistenceFailure(
	ctx context.Context,
	instanceID string,
	target managed.Release,
	stage string,
	persistErr error,
	emit func(string, string, string) error,
) Result {
	recoveryCtx, cancel := engine.recoveryContext(ctx)
	defer cancel()

	cause := fmt.Errorf("persist %s stage: %w", stage, persistErr)
	if err := engine.Deployment.Resume(recoveryCtx, instanceID); err != nil {
		cause = errors.Join(cause, fmt.Errorf("resume current release: %w", err))
	} else if err := engine.Deployment.Verify(recoveryCtx, instanceID); err != nil {
		cause = errors.Join(cause, fmt.Errorf("verify current release: %w", err))
	}
	_ = emit(stage, "failed", cause.Error())

	return Result{Status: StatusFailed, Target: target, Error: cause.Error()}
}

func (engine Engine) recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := engine.RecoveryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (engine Engine) now() time.Time {
	if engine.Now != nil {
		return engine.Now()
	}

	return time.Now()
}
