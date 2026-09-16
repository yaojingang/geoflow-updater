package operation

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/update"
)

type boundaryDeployment struct {
	fakeDeployment
	manager   *Manager
	verifyErr error
}

func (d *boundaryDeployment) Resume(ctx context.Context, id string) error {
	if d.manager != nil {
		saved, err := d.manager.Current(id)
		if err != nil {
			return err
		}
		if saved.CurrentStage != "resume" || len(saved.Stages) == 0 || saved.Stages[len(saved.Stages)-1].Status != "running" || saved.Stages[len(saved.Stages)-1].Message != "service resume boundary v1" {
			return errors.New("resume reached without a persisted running boundary")
		}
	}
	return d.fakeDeployment.Resume(ctx, id)
}

func (d *boundaryDeployment) Verify(ctx context.Context, id string) error {
	d.fakeDeployment.Verify(ctx, id)
	return d.verifyErr
}

func TestRollbackVerificationFailureNeverRestoresAcceptedWritesAgain(t *testing.T) {
	d := &boundaryDeployment{verifyErr: errors.New("health check unavailable")}
	m := &Manager{StateDir: t.TempDir(), Deployment: d}
	d.manager = m
	started, err := m.StartRollback("primary", "20260827T123456Z-1234abcd")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := m.Get("primary", started.ID)
	if err != nil || first.Status != StatusRecoveryRequired {
		t.Fatalf("first result = %+v, %v", first, err)
	}
	if got := d.snapshotCalls(); !reflect.DeepEqual(got, []string{"validate", "quiesce", "rollback", "resume", "verify"}) {
		t.Fatalf("initial calls = %v", got)
	}
	// A fresh manager models a service restart after the first Resume accepted writes.
	restarted := &Manager{StateDir: m.StateDir, Deployment: d}
	d.manager = restarted
	if err := restarted.Reconcile("primary"); err == nil {
		t.Fatal("verification failure was hidden")
	}
	want := []string{"validate", "quiesce", "rollback", "resume", "verify", "resume", "verify"}
	if got := d.snapshotCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery overwrote accepted writes: calls = %v, want %v", got, want)
	}
}

func TestUpdateVerificationFailureRetriesOnlyServices(t *testing.T) {
	d := &boundaryDeployment{verifyErr: errors.New("health check unavailable")}
	m := &Manager{StateDir: t.TempDir(), Deployment: d, Engine: update.Engine{Deployment: d}}
	d.manager = m
	if _, err := m.StartUpdate("primary"); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := m.Current("primary")
	if err != nil || current.Status != StatusRecoveryRequired {
		t.Fatalf("initial status=%s operation error=%s read error=%v", current.Status, current.Error, err)
	}
	initialCalls := d.snapshotCalls()
	d.verifyErr = nil
	restarted := &Manager{StateDir: m.StateDir, Deployment: d}
	d.manager = restarted
	if err := restarted.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	current, err = restarted.Current("primary")
	if err != nil || current.Status != StatusSucceeded {
		t.Fatalf("reconciled status=%s error=%v", current.Status, err)
	}
	if calls := d.snapshotCalls()[len(initialCalls):]; !reflect.DeepEqual(calls, []string{"resume", "verify"}) {
		t.Fatalf("recovery replayed an applied update: %v", calls)
	}
}

func boundaryOperation(kind Kind, stage, state string) Operation {
	return Operation{SchemaVersion: 1, ID: "20260827T123456.000000000Z-0011223344556677", InstanceID: "primary", Kind: kind,
		Status: StatusRecoveryRequired, CurrentStage: stage, RecoveryPointID: "20260827T123456Z-1234abcd",
		Stages: []update.Stage{{Name: stage, Status: state}}, StartedAt: time.Now().UTC()}
}

func TestReconcilePersistsResumeBoundaryForEveryNewCoordinatorState(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate, KindBackup} {
		for _, stage := range []string{"resume", "verify"} {
			for _, state := range []string{"running", "failed", "succeeded"} {
				t.Run(string(kind)+"/"+stage+"/"+state, func(t *testing.T) {
					d := &boundaryDeployment{}
					m := &Manager{StateDir: t.TempDir(), Deployment: d}
					d.manager = m
					op := boundaryOperation(kind, stage, state)
					op.Stages = append([]update.Stage{{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage}}, op.Stages...)
					if err := m.save(&op); err != nil {
						t.Fatal(err)
					}
					if err := m.Reconcile("primary"); err != nil {
						t.Fatal(err)
					}
					if got := d.snapshotCalls(); !reflect.DeepEqual(got, []string{"resume", "verify"}) {
						t.Fatalf("calls = %v", got)
					}
				})
			}
		}
	}
}

func TestReconcileLeavesLegacyUncertainRestoreForExplicitRecovery(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate} {
		for _, stage := range []string{"activate", "rollback"} {
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				d := &boundaryDeployment{}
				m := &Manager{StateDir: t.TempDir(), Deployment: d}
				op := boundaryOperation(kind, stage, "failed")
				if err := m.save(&op); err != nil {
					t.Fatal(err)
				}
				if err := m.Reconcile("primary"); err == nil {
					t.Fatal("legacy recovery uncertainty was silently accepted")
				}
				if got := d.snapshotCalls(); len(got) != 0 {
					t.Fatalf("legacy record caused deployment mutations: %v", got)
				}
				current, err := m.Current("primary")
				if err != nil || current.Status != StatusRecoveryRequired {
					t.Fatalf("current = %+v, %v", current, err)
				}
			})
		}
	}
}

func TestReconcileDoesNotResumeWhenBoundaryCannotBePersisted(t *testing.T) {
	d := &boundaryDeployment{}
	m := &Manager{StateDir: t.TempDir(), Deployment: d}
	op := boundaryOperation(KindRollback, "verify", "failed")
	op.Stages = append([]update.Stage{{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage}}, op.Stages...)
	if err := m.save(&op); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.currentPath("primary")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.currentPath("primary"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.reconcileOperation(context.Background(), &op); err == nil {
		t.Fatal("boundary persistence failure was hidden")
	}
	if got := d.snapshotCalls(); len(got) != 0 {
		t.Fatalf("services opened without durable boundary: %v", got)
	}
}

func TestRollbackPartialResumeFailureOnlyRetriesServices(t *testing.T) {
	d := &boundaryDeployment{fakeDeployment: fakeDeployment{resumeErr: errors.New("partially started")}}
	m := &Manager{StateDir: t.TempDir(), Deployment: d}
	d.manager = m
	if _, err := m.StartRollback("primary", "20260827T123456Z-1234abcd"); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.resumeErr = nil
	if err := m.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	want := []string{"validate", "quiesce", "rollback", "resume", "resume", "verify"}
	if got := d.snapshotCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestExplicitRollbackCanReplaceAnUncertainLegacyRecovery(t *testing.T) {
	d := &boundaryDeployment{}
	m := &Manager{StateDir: t.TempDir(), Deployment: d}
	d.manager = m
	old := boundaryOperation(KindRollback, "rollback", "failed")
	if err := m.save(&old); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile("primary"); err == nil {
		t.Fatal("legacy recovery was accepted")
	}
	started, err := m.StartRollback("primary", "20260827T123456Z-1234abcd")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if started.ID == old.ID {
		t.Fatal("explicit recovery reused the uncertain transaction")
	}
	current, err := m.Current("primary")
	if err != nil || current.Status != StatusSucceeded {
		t.Fatalf("current = %+v, %v", current, err)
	}
	want := []string{"validate", "quiesce", "rollback", "resume", "verify"}
	if got := d.snapshotCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestReconcileUsesResumeEvidenceFromEarlierStages(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate} {
		t.Run(string(kind), func(t *testing.T) {
			d := &boundaryDeployment{}
			m := &Manager{StateDir: t.TempDir(), Deployment: d}
			d.manager = m
			op := boundaryOperation(kind, "rollback", "failed")
			op.Stages = append([]update.Stage{{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage}}, op.Stages...)
			if err := m.save(&op); err != nil {
				t.Fatal(err)
			}
			if err := m.Reconcile("primary"); err != nil {
				t.Fatal(err)
			}
			if got := d.snapshotCalls(); !reflect.DeepEqual(got, []string{"resume", "verify"}) {
				t.Fatalf("calls = %v", got)
			}
		})
	}
}

func TestReconciledServiceRecoveryDoesNotClaimAnUnperformedUpdate(t *testing.T) {
	d := &boundaryDeployment{}
	m := &Manager{StateDir: t.TempDir(), Deployment: d}
	d.manager = m
	op := boundaryOperation(KindUpdate, "quiesce", "failed")
	op.Stages = append([]update.Stage{{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage}, {Name: "verify", Status: "succeeded"}}, op.Stages...)
	if err := m.save(&op); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	current, err := m.Current("primary")
	if err != nil || current.Status != StatusFailed {
		t.Fatalf("failed update was reported as applied: status=%s err=%v", current.Status, err)
	}
	if got := d.snapshotCalls(); !reflect.DeepEqual(got, []string{"resume", "verify"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestRepeatedRecoveryStaysWithinBridgeHistoryLimitAndRetainsOutcome(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		kind     Kind
		restored bool
		want     Status
	}{
		{"manual-restore", KindRollback, true, StatusSucceeded},
		{"applied-update", KindUpdate, false, StatusSucceeded},
		{"rolled-back-update", KindUpdate, true, StatusRolledBack},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Now().UTC()
			d := &boundaryDeployment{verifyErr: errors.New("health check remains unavailable")}
			stateDir := t.TempDir()
			m := &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
			op := boundaryOperation(scenario.kind, "verify", "failed")
			op.Stages = []update.Stage{{Name: "activate", Status: "succeeded"}}
			if scenario.restored {
				op.Stages = append(op.Stages, update.Stage{Name: "rollback", Status: "succeeded"})
			}
			op.Stages = append(op.Stages, update.Stage{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage}, update.Stage{Name: "verify", Status: "failed"})
			if err := m.save(&op); err != nil {
				t.Fatal(err)
			}
			maximumStages := 0
			for attempt := 0; attempt < 35; attempt++ {
				now = now.Add(time.Hour)
				m = &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
				d.manager = m
				if err := m.Reconcile("primary"); err == nil {
					t.Fatal("verification failure was hidden")
				}
				current, err := m.Current("primary")
				if err != nil {
					t.Fatal(err)
				}
				if current.Status != StatusRecoveryRequired {
					t.Fatalf("status = %s", current.Status)
				}
				if len(current.Stages) > maximumStages {
					maximumStages = len(current.Stages)
				}
			}
			if maximumStages > 100 {
				t.Errorf("Core bridge rejects %d stages after repeated recovery; maximum is 100", maximumStages)
			}
			d.verifyErr = nil
			now = now.Add(time.Hour)
			if err := m.Reconcile("primary"); err != nil {
				t.Fatal(err)
			}
			current, err := m.Current("primary")
			if err != nil || current.Status != scenario.want {
				t.Fatalf("lost original completion evidence: status=%s want=%s error=%v", current.Status, scenario.want, err)
			}
			if len(current.Stages) > 100 {
				t.Errorf("completed operation has %d stages", len(current.Stages))
			}
			calls := d.snapshotCalls()
			if len(calls) != 72 {
				t.Fatalf("expected 36 resume/verify attempts, got %v", calls)
			}
			for _, call := range calls {
				if call != "resume" && call != "verify" {
					t.Fatalf("data recovery replayed during retries: %v", calls)
				}
			}
		})
	}
}

func TestLegacyPartialRestoreRemainsHeldAcrossRestarts(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate} {
		t.Run(string(kind), func(t *testing.T) {
			now := time.Now().UTC()
			d := &boundaryDeployment{}
			stateDir := t.TempDir()
			m := &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
			op := boundaryOperation(kind, "verify", "failed")
			op.Stages = []update.Stage{
				{Name: "activate", Status: "succeeded"},
				{Name: "rollback", Status: "succeeded"},
				{Name: "resume", Status: "running"},
				{Name: "resume", Status: "succeeded"},
				{Name: "verify", Status: "failed"},
			}
			op.ReconcileAttempts = 1
			op.Error = "updater service could not recover an interrupted operation: recover interrupted " + string(kind) + ": restore storage: partial restore"
			if err := m.save(&op); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 3; attempt++ {
				now = now.Add(time.Hour)
				m = &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
				d.manager = m
				err := m.Reconcile("primary")
				if err == nil || !strings.Contains(err.Error(), "explicit data recovery") {
					t.Errorf("restart %d accepted uncertain restored data: %v", attempt, err)
				}
				current, err := m.Current("primary")
				if err != nil {
					t.Fatal(err)
				}
				if current.Status != StatusRecoveryRequired {
					t.Errorf("restart %d status = %s", attempt, current.Status)
				}
				if calls := d.snapshotCalls(); len(calls) != 0 {
					t.Errorf("restart %d opened uncertain data: %v", attempt, calls)
				}
				if attempt == 0 {
					// Another diagnostic can replace the top-level error. The durable hold must survive.
					current.Error = "secondary diagnostic unavailable"
					for index := 0; index < 140; index++ {
						current.Stages = append(current.Stages, update.Stage{Name: "verify", Status: "failed"})
					}
					if err := m.save(&current); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestLegacyRestoreCrashBeforeAttemptSaveNeverTrustsOldResume(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate} {
		t.Run(string(kind), func(t *testing.T) {
			now := time.Now().UTC()
			d := &fakeDeployment{} // Healthy services cannot prove a partially restored database is complete.
			stateDir := t.TempDir()
			m := &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
			op := boundaryOperation(kind, "verify", "failed")
			op.Stages = []update.Stage{
				{Name: "activate", Status: "succeeded"},
				{Name: "rollback", Status: "succeeded"},
				{Name: "resume", Status: "running"},
				{Name: "resume", Status: "succeeded"},
				{Name: "verify", Status: "failed"},
			}
			// The old coordinator could start a second restore and crash before
			// persisting its attempt or error. Only the first resume remains visible.
			if err := m.save(&op); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 3; attempt++ {
				now = now.Add(time.Hour)
				m = &Manager{StateDir: stateDir, Deployment: d, Now: func() time.Time { return now }}
				if err := m.Reconcile("primary"); err == nil || !strings.Contains(err.Error(), "explicit data recovery") {
					t.Errorf("restart %d trusted ambiguous legacy history: %v", attempt, err)
				}
				current, err := m.Current("primary")
				if err != nil || current.Status != StatusRecoveryRequired {
					t.Fatalf("restart %d status=%s error=%v", attempt, current.Status, err)
				}
				if calls := d.snapshotCalls(); len(calls) != 0 {
					t.Errorf("restart %d invoked deployment with uncertain data: %v", attempt, calls)
				}
			}
		})
	}
}

func TestResumeBoundarySurvivesUnrelatedStageCompaction(t *testing.T) {
	for _, kind := range []Kind{KindRollback, KindUpdate} {
		t.Run(string(kind), func(t *testing.T) {
			d := &boundaryDeployment{}
			m := &Manager{StateDir: t.TempDir(), Deployment: d}
			d.manager = m
			op := boundaryOperation(kind, "verify", "failed")
			op.Stages = []update.Stage{
				{Name: "activate", Status: "succeeded"},
				{Name: "resume", Status: "running"},
				{Name: "resume", Status: "running", Message: update.ResumeBoundaryMessage},
			}
			for index := 0; index < 140; index++ {
				op.Stages = append(op.Stages, update.Stage{Name: "verify", Status: "failed"})
			}
			if err := m.save(&op); err != nil {
				t.Fatal(err)
			}
			current, err := m.Current("primary")
			if err != nil || len(current.Stages) > 100 {
				t.Fatalf("stages=%d error=%v", len(current.Stages), err)
			}
			if err := m.Reconcile("primary"); err != nil {
				t.Fatalf("compaction lost the explicit resume boundary: %v", err)
			}
			if calls := d.snapshotCalls(); !reflect.DeepEqual(calls, []string{"resume", "verify"}) {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}
