package operation

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type fakeDeployment struct {
	mu          sync.Mutex
	calls       []string
	verifyStart chan struct{}
	verifyWait  chan struct{}
}

func (deployment *fakeDeployment) record(call string) {
	deployment.mu.Lock()
	defer deployment.mu.Unlock()
	deployment.calls = append(deployment.calls, call)
}

func (deployment *fakeDeployment) Resolve(context.Context, string) (managed.Release, error) {
	deployment.record("resolve")
	return managed.Release{Sequence: 18, Version: "2.5.0"}, nil
}

func (deployment *fakeDeployment) Preflight(context.Context, string, managed.Release) error {
	deployment.record("preflight")
	return nil
}

func (deployment *fakeDeployment) Pull(context.Context, string, managed.Release) error {
	deployment.record("pull")
	return nil
}

func (deployment *fakeDeployment) Quiesce(context.Context, string) error {
	deployment.record("quiesce")
	return nil
}

func (deployment *fakeDeployment) CreateRecoveryPoint(context.Context, string, string) (string, error) {
	deployment.record("backup")
	return "20260827T123456Z-1234abcd", nil
}

func (deployment *fakeDeployment) Migrate(context.Context, string, managed.Release) error {
	deployment.record("migrate")
	return nil
}

func (deployment *fakeDeployment) Activate(context.Context, string, managed.Release) error {
	deployment.record("activate")
	return nil
}

func (deployment *fakeDeployment) Rollback(context.Context, string, string) error {
	deployment.record("rollback")
	return nil
}

func (deployment *fakeDeployment) Resume(context.Context, string) error {
	deployment.record("resume")
	return nil
}

func (deployment *fakeDeployment) Verify(context.Context, string) error {
	deployment.record("verify")
	if deployment.verifyStart != nil {
		select {
		case deployment.verifyStart <- struct{}{}:
		default:
		}
	}
	if deployment.verifyWait != nil {
		<-deployment.verifyWait
	}
	return nil
}

func (deployment *fakeDeployment) ListRecoveryPoints(string) ([]recovery.Point, error) {
	return []recovery.Point{}, nil
}

func (deployment *fakeDeployment) snapshotCalls() []string {
	deployment.mu.Lock()
	defer deployment.mu.Unlock()
	return append([]string(nil), deployment.calls...)
}

func TestManagerPersistsUpdateRecoveryPointBeforeCompleting(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	manager := &Manager{
		StateDir:   t.TempDir(),
		Deployment: deployment,
		Engine:     update.Engine{Deployment: deployment},
		Now:        func() time.Time { return time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC) },
	}
	started, err := manager.StartUpdate("primary")
	if err != nil {
		t.Fatalf("StartUpdate() error = %v", err)
	}
	current := waitForCompletion(t, manager, started.ID)
	if current.Status != StatusSucceeded || current.RecoveryPointID != "20260827T123456Z-1234abcd" || current.TargetVersion != "2.5.0" {
		t.Fatalf("current operation = %#v", current)
	}
	if current.CompletedAt == nil || len(current.Stages) == 0 {
		t.Fatalf("operation did not persist completion: %#v", current)
	}
}

func TestManagerRejectsConcurrentOperationsForTheSameInstance(t *testing.T) {
	t.Parallel()

	startedVerify := make(chan struct{}, 1)
	releaseVerify := make(chan struct{})
	deployment := &fakeDeployment{verifyStart: startedVerify, verifyWait: releaseVerify}
	manager := &Manager{StateDir: t.TempDir(), Deployment: deployment}
	started, err := manager.StartVerify("primary")
	if err != nil {
		t.Fatalf("StartVerify() error = %v", err)
	}
	select {
	case <-startedVerify:
	case <-time.After(2 * time.Second):
		t.Fatal("verify operation did not start")
	}
	if _, err := manager.StartBackup("primary"); !errors.Is(err, ErrActive) {
		t.Fatalf("concurrent operation error = %v, want ErrActive", err)
	}
	close(releaseVerify)
	if current := waitForCompletion(t, manager, started.ID); current.Status != StatusSucceeded {
		t.Fatalf("verify status = %s", current.Status)
	}
}

func TestManagerReconcilesInterruptedProtectedOperationWithRollback(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	manager := &Manager{StateDir: t.TempDir(), Deployment: deployment}
	operation := Operation{
		SchemaVersion:   1,
		ID:              "20260827T123456.000000000Z-0011223344556677",
		InstanceID:      "primary",
		Kind:            KindUpdate,
		Status:          StatusRunning,
		CurrentStage:    "activate",
		RecoveryPointID: "20260827T123456Z-1234abcd",
		StartedAt:       time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC),
	}
	if err := manager.save(&operation); err != nil {
		t.Fatalf("save interrupted operation: %v", err)
	}
	if err := manager.Reconcile("primary"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	current, err := manager.Current("primary")
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.Status != StatusRolledBack || current.CurrentStage != "reconciled" || current.CompletedAt == nil {
		t.Fatalf("reconciled operation = %#v", current)
	}
	want := []string{"rollback", "resume", "verify"}
	if calls := deployment.snapshotCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestManagerResumesAnInterruptedBackupWithoutRestoringOldData(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	manager := &Manager{StateDir: t.TempDir(), Deployment: deployment}
	operation := Operation{
		SchemaVersion:   1,
		ID:              "20260827T123456.000000000Z-0011223344556677",
		InstanceID:      "primary",
		Kind:            KindBackup,
		Status:          StatusRunning,
		CurrentStage:    "resume",
		RecoveryPointID: "20260827T123456Z-1234abcd",
		StartedAt:       time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC),
	}
	if err := manager.save(&operation); err != nil {
		t.Fatalf("save interrupted backup: %v", err)
	}
	if err := manager.Reconcile("primary"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	current, err := manager.Current("primary")
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.Status != StatusSucceeded {
		t.Fatalf("reconciled backup = %#v", current)
	}
	want := []string{"resume", "verify"}
	if calls := deployment.snapshotCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestManagerCommitsAnInterruptedUpdateWhoseSuccessStageWasPersisted(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	manager := &Manager{StateDir: t.TempDir(), Deployment: deployment}
	operation := Operation{
		SchemaVersion:   1,
		ID:              "20260827T123456.000000000Z-0011223344556677",
		InstanceID:      "primary",
		Kind:            KindUpdate,
		Status:          StatusRunning,
		CurrentStage:    "succeeded",
		RecoveryPointID: "20260827T123456Z-1234abcd",
		Stages:          []update.Stage{{Name: "succeeded", Status: "succeeded"}},
		StartedAt:       time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC),
	}
	if err := manager.save(&operation); err != nil {
		t.Fatalf("save interrupted update: %v", err)
	}
	if err := manager.Reconcile("primary"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	current, err := manager.Current("primary")
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.Status != StatusSucceeded {
		t.Fatalf("reconciled update = %#v", current)
	}
	want := []string{"resume", "verify"}
	if calls := deployment.snapshotCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func waitForCompletion(t *testing.T, manager *Manager, operationID string) Operation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := manager.Current("primary")
		if err == nil && current.ID == operationID && current.CompletedAt != nil {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not complete")
	return Operation{}
}
