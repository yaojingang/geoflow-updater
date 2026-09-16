package update

import (
	"context"
	"errors"
	"fmt"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type Status string

const (
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
)

// ResumeBoundaryMessage identifies a journal written by the coordinator that
// never restores data after this boundary. Older resume stages are ambiguous:
// an older coordinator may have started another restore without recording it.
const ResumeBoundaryMessage = "service resume boundary v1"

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
	Rollback(context.Context, string, recovery.RestoreRequest) error
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
	return engine.RunWithOptions(ctx, instanceID, Options{AllowMaintenance: true}, observe)
}

func (engine Engine) RunWithOptions(ctx context.Context, instanceID string, options Options, observe Observer) Result {
	if engine.Deployment == nil {
		return Result{Status: StatusFailed, Error: "deployment service is unavailable"}
	}
	emit := func(name string, status string, message string) error {
		if name == "resume" && status == "running" {
			message = ResumeBoundaryMessage
		}
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
	if len(target.UpgradePlan) > 0 {
		if planned, ok := engine.Deployment.(PlannedDeployment); ok {
			return planned.ExecuteRelease(ctx, instanceID, target, options, observe)
		}
		return fail("preflight", errors.New("deployment does not support signed upgrade plans"))
	}
	if (!options.AllowMaintenance && !options.LegacyRequest) || options.ExpectedPlanSHA256 != "" {
		return fail("preflight", errors.New("legacy release requires an explicit maintenance update without a plan hash"))
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
		if resumeErr := engine.resumeAndVerify(recoveryCtx, instanceID, emit); resumeErr != nil {
			return Result{Status: StatusRecoveryRequired, Target: target, Error: errors.Join(err, resumeErr).Error()}
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
		if resumeErr := engine.resume(recoveryCtx, instanceID, emit); resumeErr != nil {
			return Result{Status: StatusRecoveryRequired, Target: target, Error: errors.Join(err, resumeErr).Error()}
		}
		return fail("backup", err)
	}
	if err := emit("backup", "succeeded", recoveryPointID); err != nil {
		return engine.rollback(ctx, instanceID, recoveryPointID, options.OperationID, target, fmt.Errorf("persist backup recovery point: %w", err), emit)
	}

	if recorder, ok := engine.Deployment.(interface {
		FreezeRecoveryCheckpoint(context.Context, string, recovery.RestoreRequest) error
	}); ok {
		if err := recorder.FreezeRecoveryCheckpoint(ctx, instanceID, recovery.RestoreRequest{PointID: recoveryPointID, TransactionID: options.OperationID}); err != nil {
			return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: fmt.Sprintf("freeze recovery checkpoint: %v", err)}
		}
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
			if step.name == "resume" || step.name == "verify" {
				return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: err.Error()}
			}
			return engine.rollback(ctx, instanceID, recoveryPointID, options.OperationID, target, fmt.Errorf("persist %s stage: %w", step.name, err), emit)
		}
		if err := step.run(); err != nil {
			if observeErr := emit(step.name, "failed", err.Error()); observeErr != nil {
				err = errors.Join(err, fmt.Errorf("persist %s failure: %w", step.name, observeErr))
			}
			if step.name == "resume" || step.name == "verify" {
				return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: err.Error()}
			}
			return engine.rollback(ctx, instanceID, recoveryPointID, options.OperationID, target, err, emit)
		}
		if err := emit(step.name, "succeeded", ""); err != nil {
			if step.name == "resume" || step.name == "verify" {
				return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: err.Error()}
			}
			return engine.rollback(ctx, instanceID, recoveryPointID, options.OperationID, target, fmt.Errorf("persist %s completion: %w", step.name, err), emit)
		}
	}

	if err := emit("succeeded", "succeeded", ""); err != nil {
		return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: fmt.Sprintf("persist update completion: %v", err)}
	}
	return Result{Status: StatusSucceeded, Target: target, RecoveryPointID: recoveryPointID}
}

func (engine Engine) rollback(
	ctx context.Context,
	instanceID string,
	recoveryPointID string,
	transactionID string,
	target managed.Release,
	cause error,
	emit func(string, string, string) error,
) Result {
	recoveryCtx, cancel := engine.recoveryContext(ctx)
	defer cancel()

	observeErr := emit("rollback", "running", "")
	quiesce := func() error {
		if recoverer, ok := engine.Deployment.(interface {
			QuiesceForRecovery(context.Context, string, string) error
		}); ok {
			return recoverer.QuiesceForRecovery(recoveryCtx, instanceID, recoveryPointID)
		}
		return engine.Deployment.Quiesce(recoveryCtx, instanceID)
	}
	if err := quiesce(); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("quiesce before automatic rollback: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := engine.Deployment.Rollback(recoveryCtx, instanceID, recovery.RestoreRequest{PointID: recoveryPointID, TransactionID: transactionID}); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("automatic rollback: %w", err))
		_ = emit("rollback", "failed", combined.Error())
		return Result{Status: StatusFailed, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := emit("rollback", "succeeded", ""); err != nil {
		combined := errors.Join(cause, observeErr, fmt.Errorf("persist completed data recovery: %w", err))
		return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
	}
	if err := engine.resumeAndVerify(recoveryCtx, instanceID, emit); err != nil {
		combined := errors.Join(cause, observeErr, err)
		return Result{Status: StatusRecoveryRequired, Target: target, RecoveryPointID: recoveryPointID, Error: combined.Error()}
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
	if err := engine.resumeAndVerify(recoveryCtx, instanceID, emit); err != nil {
		return Result{Status: StatusRecoveryRequired, Target: target, Error: errors.Join(cause, err).Error()}
	}
	_ = emit(stage, "failed", cause.Error())

	return Result{Status: StatusFailed, Target: target, Error: cause.Error()}
}

// Persist the boundary before any service can accept writes. This uses existing
// v1 stages so older readers can still decode the operation journal.
func (engine Engine) resume(ctx context.Context, instanceID string, emit func(string, string, string) error) error {
	if err := emit("resume", "running", ""); err != nil {
		return fmt.Errorf("persist resume boundary: %w", err)
	}
	if err := engine.Deployment.Resume(ctx, instanceID); err != nil {
		return errors.Join(err, emit("resume", "failed", err.Error()))
	}
	if err := emit("resume", "succeeded", ""); err != nil {
		return fmt.Errorf("persist resume completion: %w", err)
	}
	return nil
}

func (engine Engine) resumeAndVerify(ctx context.Context, instanceID string, emit func(string, string, string) error) error {
	if err := engine.resume(ctx, instanceID, emit); err != nil {
		return err
	}
	if err := emit("verify", "running", ""); err != nil {
		return fmt.Errorf("persist verification stage: %w", err)
	}
	if err := engine.Deployment.Verify(ctx, instanceID); err != nil {
		return errors.Join(err, emit("verify", "failed", err.Error()))
	}
	if err := emit("verify", "succeeded", ""); err != nil {
		return fmt.Errorf("persist verification completion: %w", err)
	}
	return nil
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
