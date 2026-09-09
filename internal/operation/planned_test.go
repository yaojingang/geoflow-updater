package operation

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type blockingPreview struct{ *fakeDeployment }

func (deployment *blockingPreview) Preview(ctx context.Context, _ string) (update.PlanSummary, error) {
	<-ctx.Done()
	return update.PlanSummary{}, ctx.Err()
}

type plannedRecovery struct {
	*fakeDeployment
	result update.Result
}

func (deployment *plannedRecovery) ReconcileRelease(context.Context, string, string) (update.Result, bool) {
	return deployment.result, true
}

func TestPlannedReconciliationPersistsRecoveryIdentity(t *testing.T) {
	for _, status := range []update.Status{update.StatusRecoveryRequired, update.StatusRolledBack, update.StatusSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			const point = "20260827T123456Z-1234abcd"
			deployment := &plannedRecovery{fakeDeployment: &fakeDeployment{}, result: update.Result{
				Status: status, RecoveryPointID: point, Target: managed.Release{Version: "3.0.1"}, Error: "interrupted after traffic opened",
			}}
			manager := &Manager{StateDir: t.TempDir(), Deployment: deployment}
			interrupted := Operation{SchemaVersion: 1, ID: "20260827T123456.000000000Z-0011223344556677",
				InstanceID: "primary", Kind: KindUpdate, Status: StatusRunning, CurrentStage: "switch", StartedAt: time.Now().UTC()}
			if err := manager.save(&interrupted); err != nil {
				t.Fatal(err)
			}
			err := manager.Reconcile("primary")
			if (err != nil) != (status == update.StatusRecoveryRequired) {
				t.Fatalf("reconcile status %s: %v", status, err)
			}
			restarted := &Manager{StateDir: manager.StateDir, Deployment: &fakeDeployment{}}
			current, err := restarted.Current("primary")
			if err != nil {
				t.Fatal(err)
			}
			if current.RecoveryPointID != point || current.TargetVersion != "3.0.1" || current.CompletedAt == nil {
				t.Fatalf("recovery identity did not survive restart: %#v", current)
			}
			if status == update.StatusRecoveryRequired {
				if current.Status != StatusRecoveryRequired || current.NextReconcileAt == nil || current.ReconcileAttempts != 1 {
					t.Fatalf("recovery backoff was not persisted: %#v", current)
				}
				if _, err := restarted.StartVerify("primary"); !errors.Is(err, ErrActive) {
					t.Fatalf("verify should remain blocked: %v", err)
				}
				started, err := restarted.StartRollback("primary", point)
				if err != nil {
					t.Fatalf("explicit recovery must remain available: %v", err)
				}
				if restored := waitForCompletion(t, restarted, started.ID); restored.Status != StatusSucceeded {
					t.Fatalf("explicit recovery failed: %#v", restored)
				}
			}
		})
	}
}

func TestPlannedReconciliationRetainsPreviouslyRecordedIdentity(t *testing.T) {
	manager := &Manager{Deployment: &plannedRecovery{fakeDeployment: &fakeDeployment{}, result: update.Result{Status: update.StatusSucceeded}}}
	operation := &Operation{Kind: KindUpdate, RecoveryPointID: "20260827T123456Z-1234abcd", TargetVersion: "3.0.1"}
	if _, err := manager.reconcileOperation(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if operation.RecoveryPointID == "" || operation.TargetVersion != "3.0.1" {
		t.Fatal("empty reconciliation output erased the recorded release identity")
	}
}

func TestPreviewDeadlineReleasesInstanceLock(t *testing.T) {
	manager := &Manager{StateDir: t.TempDir(), Deployment: &blockingPreview{&fakeDeployment{}}, PreviewTimeout: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if _, err := manager.Preview(ctx, "primary"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preview error = %v, want deadline exceeded", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("preview failed to apply its own deadline")
	}
	lock, err := manager.acquireLock("primary")
	if err != nil {
		t.Fatalf("preview left the instance locked: %v", err)
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
