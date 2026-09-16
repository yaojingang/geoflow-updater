package deployment

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

func TestMaintenanceRecoveryPersistsBoundaryBeforeSourceResume(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-checkpoint", true: "restored-checkpoint"}[checkpoint], func(t *testing.T) {
			service, legacy, _, tx := recoveryTopologyFixture(t)
			tx.Strategy, tx.Stage, tx.TrafficOpened = managed.StrategyMaintenance, "upgrade", false
			if !checkpoint {
				tx.RecoveryPointID = ""
			}
			restores, resumes := 0, 0
			var crashState []byte
			service.Recoveries = &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &legacy}, restore: func() error { restores++; return nil }}
			service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
				if strings.Contains(strings.Join(args, " "), "artisan up") {
					resumes++
					var err error
					crashState, err = os.ReadFile(service.transactionPath(legacy.ID))
					if err != nil {
						t.Fatal(err)
					}
					saved, err := service.readTransaction(legacy.ID)
					if err != nil || !saved.TrafficOpened || saved.Stage != "resume" {
						t.Errorf("source resume has no durable boundary: stage=%s opened=%v err=%v", saved.Stage, saved.TrafficOpened, err)
					}
					return errors.New("interrupted after source may accept writes")
				}
				return nil
			})
			result := service.failRelease(context.Background(), &tx, errors.New("upgrade failed"), nil)
			if result.Status != update.StatusRecoveryRequired || resumes != 1 {
				t.Fatalf("first recovery status=%s error=%s resumes=%d", result.Status, result.Error, resumes)
			}
			if err := os.WriteFile(service.transactionPath(legacy.ID), crashState, 0600); err != nil {
				t.Fatal(err)
			}
			result, handled := service.ReconcileRelease(context.Background(), legacy.ID, tx.OperationID)
			if !handled || result.Status != update.StatusRecoveryRequired {
				t.Fatalf("restarted recovery status=%s error=%s handled=%v", result.Status, result.Error, handled)
			}
			expected := 0
			if checkpoint {
				expected = 1
			}
			if restores != expected {
				t.Fatalf("restored data again after source resume: %d", restores)
			}
		})
	}
}

func TestMaintenanceRecoveryCannotResumeWithoutPersistingBoundary(t *testing.T) {
	service, legacy, _, tx := recoveryTopologyFixture(t)
	tx.Strategy, tx.Stage, tx.TrafficOpened = managed.StrategyMaintenance, "upgrade", false
	resumes := 0
	service.Recoveries = &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &legacy}, restore: func() error {
		if err := os.Remove(service.transactionPath(legacy.ID)); err != nil {
			return err
		}
		return os.Mkdir(service.transactionPath(legacy.ID), 0700)
	}}
	service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "artisan up") {
			resumes++
			return errors.New("resume reached")
		}
		return nil
	})
	result := service.failRelease(context.Background(), &tx, errors.New("upgrade failed"), nil)
	if result.Status != update.StatusRecoveryRequired || resumes != 0 {
		t.Fatalf("opened services without boundary: status=%s error=%s resumes=%d", result.Status, result.Error, resumes)
	}
}

func TestMaintenanceLegacyRecoveryWithoutResumeEvidenceRequiresExplicitRestore(t *testing.T) {
	service, legacy, _, tx := recoveryTopologyFixture(t)
	tx.Strategy, tx.Stage, tx.TrafficOpened = managed.StrategyMaintenance, "upgrade", false
	if err := service.saveTransaction(&tx); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.Recoveries = &topologyRecoveryStore{restore: func() error { calls++; return errors.New("restore reached") }}
	service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, _ ...string) error { calls++; return nil })
	result, handled := service.ReconcileRelease(context.Background(), legacy.ID, tx.OperationID)
	if !handled || result.Status != update.StatusRecoveryRequired || !strings.Contains(result.Error, "explicit") || calls != 0 {
		t.Fatalf("unsafe legacy recovery: status=%s error=%s handled=%v calls=%d", result.Status, result.Error, handled, calls)
	}
}

func TestMaintenanceLegacyUnpersistedCheckpointRemainsHeldAcrossRestarts(t *testing.T) {
	service, legacy, _, tx := recoveryTopologyFixture(t)
	// The old failure handler ignored a journal save failure before restoring.
	// A created checkpoint and a partially completed restore can both be absent
	// from the last durable backup-stage record.
	tx.Strategy, tx.Stage, tx.TrafficOpened = managed.StrategyMaintenance, "backup", false
	tx.RecoveryPointID, tx.Error = "", ""
	if err := service.saveTransaction(&tx); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.Recoveries = &topologyRecoveryStore{restore: func() error { calls++; return nil }}
	service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, _ ...string) error { calls++; return nil })
	for attempt := 0; attempt < 3; attempt++ {
		result, handled := service.ReconcileRelease(context.Background(), legacy.ID, tx.OperationID)
		if !handled || result.Status != update.StatusRecoveryRequired || !strings.Contains(result.Error, "explicit") || calls != 0 {
			t.Fatalf("restart %d accepted ambiguous maintenance data: status=%s error=%s handled=%v calls=%d", attempt, result.Status, result.Error, handled, calls)
		}
	}
}

func TestQuiesceFailureLeavesResumeToTheOperationJournal(t *testing.T) {
	service, legacy, _, _ := recoveryTopologyFixture(t)
	resumes := 0
	failure := errors.New("retired worker cannot drain")
	service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
		cmd := strings.Join(args, " ")
		if strings.Contains(cmd, "name=^/geoflow-system-update-queue-prod$") {
			return failure
		}
		if strings.Contains(cmd, "artisan up") {
			resumes++
		}
		return nil
	})
	err := service.Quiesce(context.Background(), legacy.ID)
	if !errors.Is(err, failure) || resumes != 0 {
		t.Fatalf("unjournaled resume after quiesce failure: error=%v resumes=%d", err, resumes)
	}
}
