package operation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

var (
	ErrActive               = errors.New("another updater operation is active")
	ErrInvalidRecoveryPoint = errors.New("recovery point identifier is invalid")
	instanceIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	recoveryIDPattern       = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}$`)
	operationIDPattern      = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{16}$`)
)

type Kind string

const (
	KindUpdate   Kind = "update"
	KindBackup   Kind = "backup"
	KindRollback Kind = "rollback"
	KindVerify   Kind = "verify"
)

type Status string

const (
	StatusQueued           Status = "queued"
	StatusRunning          Status = "running"
	StatusSucceeded        Status = "succeeded"
	StatusFailed           Status = "failed"
	StatusRolledBack       Status = "rolled_back"
	StatusRecoveryRequired Status = "recovery_required"
)

type Operation struct {
	SchemaVersion     int            `json:"schema_version"`
	ID                string         `json:"id"`
	InstanceID        string         `json:"instance_id"`
	Kind              Kind           `json:"kind"`
	Status            Status         `json:"status"`
	CurrentStage      string         `json:"current_stage,omitempty"`
	Stages            []update.Stage `json:"stages"`
	TargetVersion     string         `json:"target_version,omitempty"`
	RecoveryPointID   string         `json:"recovery_point_id,omitempty"`
	Error             string         `json:"error,omitempty"`
	StartedAt         time.Time      `json:"started_at"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	ReconcileAttempts int            `json:"reconcile_attempts,omitempty"`
	NextReconcileAt   *time.Time     `json:"next_reconcile_at,omitempty"`
}

type Deployment interface {
	update.Deployment
	QuiesceForRecovery(context.Context, string, string) error
	ValidateRecoveryPoint(string, string) error
	ListRecoveryPoints(string) ([]recovery.Point, error)
}

type Manager struct {
	StateDir         string
	Context          context.Context
	Engine           update.Engine
	Deployment       Deployment
	Now              func() time.Time
	Random           io.Reader
	OperationTimeout time.Duration
	RecoveryTimeout  time.Duration
	mu               sync.Mutex
	active           map[string]string
	wg               sync.WaitGroup
}

func (manager *Manager) StartUpdate(instanceID string) (Operation, error) {
	return manager.start(instanceID, KindUpdate, "", func(ctx context.Context, operation *Operation, save func() error) {
		result := manager.Engine.Run(ctx, instanceID, func(stage update.Stage) error {
			if stage.Name == "backup" && stage.Status == "succeeded" && stage.Message != "" {
				operation.RecoveryPointID = stage.Message
			}
			return manager.updateStage(operation, stage, save)
		})
		operation.RecoveryPointID = result.RecoveryPointID
		operation.TargetVersion = result.Target.Version
		operation.Error = result.Error
		switch result.Status {
		case update.StatusSucceeded:
			operation.Status = StatusSucceeded
		case update.StatusRolledBack:
			operation.Status = StatusRolledBack
		default:
			if updateRecoveryNeeded(*operation) {
				operation.Status = StatusRecoveryRequired
			} else {
				operation.Status = StatusFailed
			}
		}
	})
}

func (manager *Manager) StartBackup(instanceID string) (Operation, error) {
	return manager.start(instanceID, KindBackup, "", func(ctx context.Context, operation *Operation, save func() error) {
		if !manager.requireDeployment(operation) {
			return
		}
		if !manager.step(ctx, operation, save, "preflight", func() error { return manager.Deployment.Verify(ctx, instanceID) }) {
			return
		}
		if !manager.step(ctx, operation, save, "quiesce", func() error { return manager.Deployment.Quiesce(ctx, instanceID) }) {
			manager.resumeAfterFailure(ctx, instanceID, operation)
			return
		}
		var recoveryPointID string
		backupOK := manager.step(ctx, operation, save, "backup", func() error {
			var err error
			recoveryPointID, err = manager.Deployment.CreateRecoveryPoint(ctx, instanceID, "manual-backup")
			return err
		})
		operation.RecoveryPointID = recoveryPointID
		resumeCtx, cancel := manager.recoveryContext(ctx)
		defer cancel()
		resumeOK := manager.step(resumeCtx, operation, save, "resume", func() error { return manager.Deployment.Resume(resumeCtx, instanceID) })
		if !resumeOK {
			operation.Status = StatusRecoveryRequired
			manager.resumeAfterFailure(ctx, instanceID, operation)
		}
		if backupOK && resumeOK && manager.step(ctx, operation, save, "verify", func() error { return manager.Deployment.Verify(ctx, instanceID) }) {
			operation.Status = StatusSucceeded
		} else if backupOK && resumeOK {
			operation.Status = StatusRecoveryRequired
		}
	})
}

func (manager *Manager) StartRollback(instanceID string, recoveryPointID string) (Operation, error) {
	if !recoveryIDPattern.MatchString(recoveryPointID) {
		return Operation{}, ErrInvalidRecoveryPoint
	}
	return manager.start(instanceID, KindRollback, recoveryPointID, func(ctx context.Context, operation *Operation, save func() error) {
		if !manager.requireDeployment(operation) {
			return
		}
		if !manager.step(ctx, operation, save, "preflight", func() error {
			return manager.Deployment.ValidateRecoveryPoint(instanceID, recoveryPointID)
		}) {
			return
		}
		if !manager.step(ctx, operation, save, "quiesce", func() error { return manager.Deployment.Quiesce(ctx, instanceID) }) {
			manager.resumeAfterFailure(ctx, instanceID, operation)
			return
		}
		if !manager.step(ctx, operation, save, "rollback", func() error { return manager.Deployment.Rollback(ctx, instanceID, recoveryPointID) }) {
			operation.Status = StatusRecoveryRequired
			return
		}
		if !manager.step(ctx, operation, save, "resume", func() error { return manager.Deployment.Resume(ctx, instanceID) }) {
			operation.Status = StatusRecoveryRequired
			return
		}
		if manager.step(ctx, operation, save, "verify", func() error { return manager.Deployment.Verify(ctx, instanceID) }) {
			operation.Status = StatusSucceeded
		} else {
			operation.Status = StatusRecoveryRequired
		}
	})
}

func (manager *Manager) StartVerify(instanceID string) (Operation, error) {
	return manager.start(instanceID, KindVerify, "", func(ctx context.Context, operation *Operation, save func() error) {
		if !manager.requireDeployment(operation) {
			return
		}
		if manager.step(ctx, operation, save, "verify", func() error { return manager.Deployment.Verify(ctx, instanceID) }) {
			operation.Status = StatusSucceeded
		}
	})
}

func (manager *Manager) Current(instanceID string) (Operation, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return Operation{}, errors.New("managed instance identifier is invalid")
	}
	return manager.read(manager.currentPath(instanceID), instanceID, "")
}

func (manager *Manager) Get(instanceID string, operationID string) (Operation, error) {
	if !instanceIDPattern.MatchString(instanceID) || !operationIDPattern.MatchString(operationID) {
		return Operation{}, errors.New("operation identifier is invalid")
	}

	return manager.read(filepath.Join(manager.operationsDir(instanceID), operationID+".json"), instanceID, operationID)
}

func (manager *Manager) read(path string, instanceID string, operationID string) (Operation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Operation{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 2*1024*1024 {
		return Operation{}, errors.New("operation state is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Operation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var operation Operation
	if err := decoder.Decode(&operation); err != nil {
		return Operation{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Operation{}, errors.New("operation state contains trailing JSON")
	}
	if operation.SchemaVersion != 1 || operation.InstanceID != instanceID || !operationIDPattern.MatchString(operation.ID) ||
		(operationID != "" && operation.ID != operationID) {
		return Operation{}, errors.New("operation state is invalid")
	}
	return operation, nil
}

func (manager *Manager) RecoveryPoints(instanceID string) ([]recovery.Point, error) {
	if manager.Deployment == nil {
		return nil, errors.New("deployment service is unavailable")
	}
	return manager.Deployment.ListRecoveryPoints(instanceID)
}

func (manager *Manager) Reconcile(instanceID string) error {
	if !instanceIDPattern.MatchString(instanceID) {
		return errors.New("managed instance identifier is invalid")
	}
	lock, err := manager.acquireLock(instanceID)
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	operation, err := manager.Current(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if operation.Status != StatusQueued && operation.Status != StatusRunning && !recoveryRequired(operation) {
		return nil
	}
	if recoveryRequired(operation) && operation.NextReconcileAt != nil && manager.now().UTC().Before(*operation.NextReconcileAt) {
		return nil
	}
	if manager.Deployment == nil {
		return errors.New("deployment service is unavailable")
	}
	ctx, cancel := manager.recoveryContext(manager.baseContext())
	defer cancel()
	status, err := manager.reconcileOperation(ctx, &operation)
	if err != nil {
		operation.Status = StatusRecoveryRequired
		operation.Error = fmt.Sprintf("updater service could not recover an interrupted operation: %v", err)
		operation.ReconcileAttempts++
		nextAttempt := manager.now().UTC().Add(reconciliationBackoff(operation.ReconcileAttempts))
		operation.NextReconcileAt = &nextAttempt
		completed := manager.now().UTC()
		operation.CompletedAt = &completed
		if saveErr := manager.save(&operation); saveErr != nil {
			return errors.Join(err, fmt.Errorf("persist failed operation recovery: %w", saveErr))
		}
		return err
	}
	operation.Status = status
	message := "updater service recovered an interrupted operation"
	operation.Error = message
	operation.CurrentStage = "reconciled"
	operation.ReconcileAttempts = 0
	operation.NextReconcileAt = nil
	completed := manager.now().UTC()
	operation.CompletedAt = &completed
	return manager.save(&operation)
}

func (manager *Manager) reconcileOperation(ctx context.Context, operation *Operation) (Status, error) {
	resumeAndVerify := func() error {
		if err := manager.Deployment.Resume(ctx, operation.InstanceID); err != nil {
			return fmt.Errorf("resume after interrupted operation: %w", err)
		}
		if err := manager.Deployment.Verify(ctx, operation.InstanceID); err != nil {
			return fmt.Errorf("verify after interrupted operation: %w", err)
		}
		return nil
	}
	lastStageStatus := ""
	if len(operation.Stages) > 0 && operation.Stages[len(operation.Stages)-1].Name == operation.CurrentStage {
		lastStageStatus = operation.Stages[len(operation.Stages)-1].Status
	}

	switch operation.Kind {
	case KindBackup:
		if err := resumeAndVerify(); err != nil {
			return StatusFailed, err
		}
		if operation.RecoveryPointID != "" {
			return StatusSucceeded, nil
		}
		return StatusFailed, nil
	case KindVerify:
		if err := manager.Deployment.Verify(ctx, operation.InstanceID); err != nil {
			return StatusFailed, fmt.Errorf("retry interrupted verification: %w", err)
		}
		return StatusSucceeded, nil
	case KindRollback:
		if (operation.CurrentStage == "resume" || operation.CurrentStage == "verify") && lastStageStatus != "failed" {
			if err := resumeAndVerify(); err == nil {
				return StatusSucceeded, nil
			}
		}
		if operation.RecoveryPointID == "" {
			return StatusFailed, errors.New("interrupted rollback has no recovery point")
		}
		if err := manager.Deployment.QuiesceForRecovery(ctx, operation.InstanceID, operation.RecoveryPointID); err != nil {
			return StatusFailed, fmt.Errorf("quiesce before recovering interrupted rollback: %w", err)
		}
		if err := manager.Deployment.Rollback(ctx, operation.InstanceID, operation.RecoveryPointID); err != nil {
			return StatusFailed, fmt.Errorf("recover interrupted rollback: %w", err)
		}
		if err := resumeAndVerify(); err != nil {
			return StatusFailed, err
		}
		return StatusSucceeded, nil
	case KindUpdate:
		if operation.CurrentStage == "succeeded" {
			if err := resumeAndVerify(); err == nil {
				return StatusSucceeded, nil
			}
		}
		if operation.CurrentStage == "rolled_back" {
			if err := resumeAndVerify(); err == nil {
				return StatusRolledBack, nil
			}
		}
		if (operation.CurrentStage == "resume" || operation.CurrentStage == "verify") && lastStageStatus != "failed" {
			if err := resumeAndVerify(); err == nil {
				return StatusSucceeded, nil
			}
		}
		if operation.RecoveryPointID == "" {
			if err := resumeAndVerify(); err != nil {
				return StatusFailed, err
			}
			return StatusFailed, nil
		}
		if operation.CurrentStage == "backup" && lastStageStatus == "succeeded" {
			if err := resumeAndVerify(); err != nil {
				return StatusFailed, err
			}
			return StatusFailed, nil
		}
		if err := manager.Deployment.QuiesceForRecovery(ctx, operation.InstanceID, operation.RecoveryPointID); err != nil {
			return StatusFailed, fmt.Errorf("quiesce before recovering interrupted update: %w", err)
		}
		if err := manager.Deployment.Rollback(ctx, operation.InstanceID, operation.RecoveryPointID); err != nil {
			return StatusFailed, fmt.Errorf("recover interrupted update: %w", err)
		}
		if err := resumeAndVerify(); err != nil {
			return StatusFailed, err
		}
		return StatusRolledBack, nil
	default:
		return StatusFailed, errors.New("interrupted operation kind is invalid")
	}
}

func (manager *Manager) start(instanceID string, kind Kind, recoveryPointID string, run func(context.Context, *Operation, func() error)) (Operation, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return Operation{}, errors.New("managed instance identifier is invalid")
	}
	manager.mu.Lock()
	if manager.active == nil {
		manager.active = make(map[string]string)
	}
	if manager.active[instanceID] != "" {
		manager.mu.Unlock()
		return Operation{}, ErrActive
	}
	current, currentErr := manager.Current(instanceID)
	if currentErr == nil && (current.Status == StatusQueued || current.Status == StatusRunning || current.Status == StatusRecoveryRequired) {
		manager.mu.Unlock()
		return Operation{}, ErrActive
	}
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		manager.mu.Unlock()
		return Operation{}, fmt.Errorf("read current operation: %w", currentErr)
	}
	lock, err := manager.acquireLock(instanceID)
	if err != nil {
		manager.mu.Unlock()
		return Operation{}, err
	}
	id, err := manager.newID()
	if err != nil {
		_ = lock.Close()
		manager.mu.Unlock()
		return Operation{}, err
	}
	operation := Operation{
		SchemaVersion:   1,
		ID:              id,
		InstanceID:      instanceID,
		Kind:            kind,
		Status:          StatusQueued,
		Stages:          []update.Stage{},
		RecoveryPointID: recoveryPointID,
		StartedAt:       manager.now().UTC(),
	}
	if err := manager.save(&operation); err != nil {
		_ = lock.Close()
		manager.mu.Unlock()
		return Operation{}, err
	}
	manager.active[instanceID] = id
	manager.wg.Add(1)
	manager.mu.Unlock()

	started := operation
	go func(operation Operation) {
		defer func() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			manager.mu.Lock()
			delete(manager.active, instanceID)
			manager.mu.Unlock()
			manager.wg.Done()
		}()
		ctx, cancel := context.WithTimeout(manager.baseContext(), manager.operationTimeout())
		defer cancel()
		operation.Status = StatusRunning
		if err := manager.save(&operation); err != nil {
			return
		}
		save := func() error { return manager.save(&operation) }
		run(ctx, &operation, save)
		if operation.Status == StatusRunning {
			operation.Status = StatusFailed
			operation.Error = "operation ended without a terminal result"
		}
		completed := manager.now().UTC()
		operation.CompletedAt = &completed
		_ = manager.save(&operation)
	}(operation)

	return started, nil
}

func (manager *Manager) Wait(ctx context.Context) error {
	completed := make(chan struct{})
	go func() {
		manager.wg.Wait()
		close(completed)
	}()

	select {
	case <-completed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *Manager) step(ctx context.Context, operation *Operation, save func() error, name string, run func() error) bool {
	if err := manager.updateStage(operation, update.Stage{Name: name, Status: "running", UpdatedAt: manager.now().UTC()}, save); err != nil {
		manager.failPersistence(operation, name, err)
		return false
	}
	if err := run(); err != nil {
		if saveErr := manager.updateStage(operation, update.Stage{Name: name, Status: "failed", Message: err.Error(), UpdatedAt: manager.now().UTC()}, save); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("persist %s failure: %w", name, saveErr))
		}
		operation.Status = StatusFailed
		operation.Error = err.Error()
		return false
	}
	if err := manager.updateStage(operation, update.Stage{Name: name, Status: "succeeded", UpdatedAt: manager.now().UTC()}, save); err != nil {
		manager.failPersistence(operation, name, err)
		return false
	}
	return true
}

func (manager *Manager) updateStage(operation *Operation, stage update.Stage, save func() error) error {
	operation.CurrentStage = stage.Name
	operation.Stages = append(operation.Stages, stage)
	return save()
}

func (manager *Manager) failPersistence(operation *Operation, stage string, err error) {
	operation.Status = StatusFailed
	operation.Error = fmt.Sprintf("persist %s stage: %v", stage, err)
}

func (manager *Manager) resumeAfterFailure(ctx context.Context, instanceID string, operation *Operation) {
	recoveryCtx, cancel := manager.recoveryContext(ctx)
	defer cancel()
	if err := manager.Deployment.Resume(recoveryCtx, instanceID); err != nil {
		operation.Error = errors.Join(errors.New(operation.Error), fmt.Errorf("resume current release: %w", err)).Error()
		operation.Status = StatusRecoveryRequired
		return
	}
	if err := manager.Deployment.Verify(recoveryCtx, instanceID); err != nil {
		operation.Error = errors.Join(errors.New(operation.Error), fmt.Errorf("verify current release: %w", err)).Error()
		operation.Status = StatusRecoveryRequired
	}
}

func (manager *Manager) requireDeployment(operation *Operation) bool {
	if manager.Deployment != nil {
		return true
	}
	operation.Status = StatusFailed
	operation.Error = "deployment service is unavailable"
	return false
}

func (manager *Manager) acquireLock(instanceID string) (*os.File, error) {
	directory := filepath.Join(manager.stateDir(), "instances", instanceID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, "operation.lock"), os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, ErrActive
	}
	return lock, nil
}

func (manager *Manager) save(operation *Operation) error {
	operation.Error = boundedOperationMessage(operation.Error)
	for index := range operation.Stages {
		operation.Stages[index].Message = boundedOperationMessage(operation.Stages[index].Message)
	}
	directory := filepath.Dir(manager.currentPath(operation.InstanceID))
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(operation, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	for _, path := range []string{
		filepath.Join(directory, operation.ID+".json"),
		manager.currentPath(operation.InstanceID),
	} {
		if err := replaceContents(path, contents); err != nil {
			return err
		}
	}
	return nil
}

func boundedOperationMessage(message string) string {
	const maximumBytes = 4096

	message = strings.ToValidUTF8(message, "�")
	if len(message) <= maximumBytes {
		return message
	}
	message = message[:maximumBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}

	return message
}

func replaceContents(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".operation-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (manager *Manager) currentPath(instanceID string) string {
	return filepath.Join(manager.operationsDir(instanceID), "current.json")
}

func (manager *Manager) operationsDir(instanceID string) string {
	return filepath.Join(manager.stateDir(), "instances", instanceID, "operations")
}

func (manager *Manager) stateDir() string {
	if manager.StateDir != "" {
		return manager.StateDir
	}
	return "/var/lib/geoflow-updater"
}

func (manager *Manager) baseContext() context.Context {
	if manager.Context != nil {
		return manager.Context
	}
	return context.Background()
}

func (manager *Manager) operationTimeout() time.Duration {
	if manager.OperationTimeout > 0 {
		return manager.OperationTimeout
	}

	return 2 * time.Hour
}

func (manager *Manager) recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := manager.RecoveryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func recoveryRequired(operation Operation) bool {
	if operation.Status == StatusRecoveryRequired {
		return true
	}
	if operation.Status != StatusFailed || operation.RecoveryPointID == "" || operation.CurrentStage != "rollback" {
		return false
	}
	if len(operation.Stages) == 0 {
		return true
	}
	last := operation.Stages[len(operation.Stages)-1]

	return last.Name == "rollback" && last.Status == "failed"
}

func reconciliationBackoff(attempts int) time.Duration {
	delay := 30 * time.Second
	for attempt := 1; attempt < attempts && delay < 15*time.Minute; attempt++ {
		delay *= 2
		if delay > 15*time.Minute {
			return 15 * time.Minute
		}
	}

	return delay
}

func updateRecoveryNeeded(operation Operation) bool {
	if operation.Kind != KindUpdate || operation.Status != StatusRunning {
		return false
	}

	return operation.CurrentStage == "quiesce" || operation.CurrentStage == "backup" || operation.CurrentStage == "rollback"
}

func (manager *Manager) now() time.Time {
	if manager.Now != nil {
		return manager.Now()
	}
	return time.Now()
}

func (manager *Manager) newID() (string, error) {
	random := manager.Random
	if random == nil {
		random = rand.Reader
	}
	suffix := make([]byte, 8)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return "", err
	}
	return manager.now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix), nil
}
