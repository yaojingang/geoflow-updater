package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
)

type coordinatedDeployment struct {
	*fakeDeployment
	baseline string
	plans    int
	muPlan   sync.Mutex
}

func (d *coordinatedDeployment) ActionSnapshot(_ context.Context, _, action, _ string) (coordination.Snapshot, error) {
	d.muPlan.Lock()
	defer d.muPlan.Unlock()
	d.plans++
	return coordination.Snapshot{BaselineSHA256: d.baseline, MaintenanceRequired: action != "switch-back", Continuation: "host_only", Details: json.RawMessage(`{"source":{"version":"3.1.0"}}`)}, nil
}

type admissionAuth struct {
	mu      sync.Mutex
	calls   int
	counter int64
}

func (a *admissionAuth) AuthorizeAdmission(_ string, _ authorization.Scope, code string, existing func() (bool, error), commit func(int64) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing != nil {
		found, err := existing()
		if found || err != nil {
			return err
		}
	}
	a.calls++
	if code != "123456" {
		return authorization.ErrInvalid
	}
	return commit(a.counter)
}
func coordinationFixture(t *testing.T, action string) (*Manager, *coordinatedDeployment, *admissionAuth, coordination.SubmitRequest) {
	t.Helper()
	state := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	a, err := (recoverycontrol.Store{StateDir: state}).Initialize("primary")
	if err != nil {
		t.Fatal(err)
	}
	d := &coordinatedDeployment{fakeDeployment: &fakeDeployment{}, baseline: strings.Repeat("a", 64)}
	m := &Manager{StateDir: state, Deployment: d, Now: func() time.Time { return now }}
	actor := coordination.Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("b", 64)}
	planRequest := coordination.PlanRequest{Action: action, ExpectedEpoch: a.State.Epoch, Actor: actor}
	if action == "restore" {
		planRequest.RecoveryPointID = "20260916T120000Z-00000001"
	}
	p, err := m.CreatePlan(context.Background(), "primary", planRequest)
	if err != nil {
		t.Fatal(err)
	}
	return m, d, &admissionAuth{counter: now.Unix() / 30}, coordination.SubmitRequest{Action: action, PlanID: p.PlanID, PlanSHA256: p.PlanSHA256, ExpectedEpoch: p.ExpectedEpoch, AllowMaintenance: true, ConfirmHostAccess: true, ClientRequestID: "request-0001", Actor: actor, Scope: coordination.WireScope(action)}
}
func TestCoordinatedDuplicateIsReadBeforeOTPPlanEpochAndActor(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "backup")
	first, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }
	_, err = (recoverycontrol.Store{StateDir: m.StateDir}).Begin("primary", "20260916T120000Z-00000001", "other-restore", strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	r.Actor.AdminID = 2
	d.muPlan.Lock()
	before := d.plans
	d.muPlan.Unlock()
	repeated, err := m.Submit(context.Background(), "primary", r, "", auth)
	if err != nil || repeated.OperationID != first.OperationID || auth.calls != 1 || repeated.BackgroundStatus != "held" {
		t.Fatalf("duplicate %#v %v calls=%d", repeated, err, auth.calls)
	}
	d.muPlan.Lock()
	defer d.muPlan.Unlock()
	if d.plans != before {
		t.Fatal("duplicate replanned")
	}
	r.AllowMaintenance = false
	if _, err = m.Submit(context.Background(), "primary", r, "", auth); !errors.Is(err, coordination.ErrConflict) {
		t.Fatal(err)
	}
}
func TestCoordinatedConcurrentSameRequestHasOneAdmission(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "backup")
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := m.Submit(context.Background(), "primary", r, "123456", auth)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- receipt.OperationID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("multiple identities")
		}
		first = id
	}
	m.Wait(context.Background())
	if auth.calls != 1 {
		t.Fatal(auth.calls)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, call := range d.calls {
		if call == "backup" {
			count++
		}
	}
	if count != 1 {
		t.Fatal(d.calls)
	}
}
func TestCoordinatedCrashRecoversIdentityWithoutDispatch(t *testing.T) {
	for _, fault := range []string{"before-write", "before-rename", "after-rename", "after-sync"} {
		t.Run(fault, func(t *testing.T) {
			m, d, auth, r := coordinationFixture(t, "backup")
			m.Coordination = &coordination.Store{StateDir: m.StateDir, Fault: func(stage string) error {
				if stage == fault {
					return errors.New("crash")
				}
				return nil
			}}
			receipt, err := m.Submit(context.Background(), "primary", r, "123456", auth)
			accepted := fault == "after-rename" || fault == "after-sync"
			if accepted && err != nil {
				t.Fatal(err)
			}
			if !accepted && err == nil {
				t.Fatal("uncommitted submission accepted")
			}
			if len(d.calls) != 0 {
				t.Fatal("side effect before durable projection", d.calls)
			}
			if !accepted {
				return
			}
			if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
				t.Fatal(err)
			}
			restarted := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
			for i := 0; i < 2; i++ {
				if err := restarted.Reconcile("primary"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := restarted.Request("primary", r.ClientRequestID)
			if err != nil || got.OperationID != receipt.OperationID {
				t.Fatal(got, err)
			}
			if len(d.calls) != 0 {
				t.Fatal("unstarted accepted operation dispatched after restart", d.calls)
			}
			repeated, err := restarted.Submit(context.Background(), "primary", r, "", auth)
			if err != nil || repeated.OperationID != receipt.OperationID {
				t.Fatal(repeated, err)
			}
		})
	}
}
func TestCoordinatedTerminalAuthorityRepairsLostProjections(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "restore")
	receipt, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	before := len(d.calls)
	if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
		t.Fatal(err)
	}
	restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
	if err := restart.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	op, err := restart.Get("primary", receipt.OperationID)
	if err != nil || op.Status != StatusSucceeded || op.RecoveryPointID == "" {
		t.Fatal(op, err)
	}
	if len(d.calls) != before {
		t.Fatal("terminal operation was replayed")
	}
	if _, err := os.Stat(filepath.Join(restart.operationsDir("primary"), "current.json")); err != nil {
		t.Fatal(err)
	}
}
func TestCoordinatedAdmissionRechecksFourActionPlans(t *testing.T) {
	for _, action := range []string{"update", "backup", "restore", "switch-back"} {
		for _, condition := range []string{"baseline", "epoch", "expired", "maintenance", "host", "actor", "scope"} {
			t.Run(action+"/"+condition, func(t *testing.T) {
				m, d, auth, r := coordinationFixture(t, action)
				switch condition {
				case "baseline":
					d.baseline = strings.Repeat("f", 64)
				case "epoch":
					_, _ = (recoverycontrol.Store{StateDir: m.StateDir}).Begin("primary", "20260916T120000Z-00000001", "other-restore", strings.Repeat("c", 64))
				case "expired":
					m.Now = func() time.Time { return time.Date(2026, 9, 16, 12, 11, 0, 0, time.UTC) }
				case "maintenance":
					if action == "switch-back" {
						return
					}
					r.AllowMaintenance = false
				case "host":
					r.ConfirmHostAccess = false
				case "actor":
					r.Actor.AdminID = 2
				case "scope":
					r.Scope = "updater:wrong"
				}
				if _, err := m.Submit(context.Background(), "primary", r, "123456", auth); err == nil {
					t.Fatal("invalid plan admitted")
				}
				if len(d.calls) != 0 {
					t.Fatal(d.calls)
				}
				if _, err := m.Request("primary", r.ClientRequestID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestAdmissionProjectionFailuresKeepReceiptAndNeverStart(t *testing.T) {
	for _, boundary := range []string{"authority-update", "operation-file", "current-file"} {
		t.Run(boundary, func(t *testing.T) {
			m, d, auth, r := coordinationFixture(t, "backup")
			if boundary == "authority-update" {
				writes := 0
				m.Coordination = &coordination.Store{StateDir: m.StateDir, Fault: func(stage string) error {
					if stage == "before-write" {
						writes++
						if writes > 1 {
							return errors.New("projection failed")
						}
					}
					return nil
				}}
			} else {
				if err := os.MkdirAll(m.operationsDir("primary"), 0750); err != nil {
					t.Fatal(err)
				}
				path := "current.json"
				if boundary == "operation-file" {
					m.Random = strings.NewReader(strings.Repeat("a", 8))
					path = "20260916T120000.000000000Z-6161616161616161.json"
				}
				if err := os.Mkdir(filepath.Join(m.operationsDir("primary"), path), 0750); err != nil {
					t.Fatal(err)
				}
				if boundary == "current-file" { // fail after current precheck, inside the admission storage boundary
					injected := false
					m.Coordination = &coordination.Store{StateDir: m.StateDir, Fault: func(stage string) error {
						if stage == "after-sync" && !injected {
							injected = true
							return os.Mkdir(filepath.Join(m.operationsDir("primary"), path), 0750)
						}
						return nil
					}}
					if err := os.Remove(filepath.Join(m.operationsDir("primary"), path)); err != nil {
						t.Fatal(err)
					}
				}
			}
			receipt, err := m.Submit(context.Background(), "primary", r, "123456", auth)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.OperationID == "" || receipt.AdmissionStatus != "pending" || receipt.Operation != nil || len(d.calls) != 0 {
				t.Fatal(receipt, d.calls)
			}
			if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
				t.Fatal(err)
			}
			restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
			if err := restart.Reconcile("primary"); err != nil {
				t.Fatal(err)
			}
			if len(d.calls) != 0 {
				t.Fatal(d.calls)
			}
		})
	}
}
func TestAdmissionSequenceRepairsCurrentAcrossClockChanges(t *testing.T) {
	m, _, auth, r := coordinationFixture(t, "backup")
	first, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	previous := m.now()
	m.Now = func() time.Time { return previous.Add(-time.Second) }
	plan, err := m.CreatePlan(context.Background(), "primary", coordination.PlanRequest{Action: r.Action, ExpectedEpoch: r.ExpectedEpoch, Actor: r.Actor})
	if err != nil {
		t.Fatal(err)
	}
	r.PlanID = plan.PlanID
	r.PlanSHA256 = plan.PlanSHA256
	r.ClientRequestID = "request-0002"
	second, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	if first.OperationID == second.OperationID {
		t.Fatal("duplicate identity")
	}
	if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	current, err := m.Current("primary")
	if err != nil || current.ID != second.OperationID {
		t.Fatal(current, err)
	}
}

func TestLegacyRecoverySupersedesAdmissionAcrossRestartsAndLostProjections(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "restore")
	d.resumeErr = errors.New("initial resume failed")
	first, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	old, err := m.Request("primary", r.ClientRequestID)
	if err != nil {
		t.Fatal(err)
	}
	original := string(old.Operation)
	d.resumeErr = nil
	previous := m.now()
	m.Now = func() time.Time { return previous.Add(-time.Hour) }
	newer, err := m.StartRollback("primary", "20260916T130000Z-00000002")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	calls := len(d.snapshotCalls())
	for attempt := 0; attempt < 3; attempt++ {
		if attempt == 1 {
			if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
				t.Fatal(err)
			}
		}
		restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
		if err := restart.Reconcile("primary"); err != nil {
			t.Fatal(err)
		}
		current, err := restart.Current("primary")
		if err != nil || current.ID != newer.ID || current.Status != StatusSucceeded {
			t.Fatalf("newer recovery lost: %#v %v", current, err)
		}
		receipt, err := restart.Request("primary", r.ClientRequestID)
		if err != nil || receipt.OperationID != first.OperationID || string(receipt.Operation) != original {
			t.Fatal("old receipt changed", receipt, err)
		}
		if len(d.snapshotCalls()) != calls {
			t.Fatal("older deployment reconciled", d.snapshotCalls()[calls:])
		}
	}
	stale, err := m.Get("primary", first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	stale.Status = StatusSucceeded
	if err := m.save(&stale); err == nil {
		t.Fatal("superseded operation may still rewrite authority")
	}
	after, _ := m.Request("primary", r.ClientRequestID)
	if string(after.Operation) != original {
		t.Fatal("stale save corrupted receipt")
	}
}

func TestUncertainLegacyHeadDoesNotResumeOldAdmission(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "restore")
	d.resumeErr = errors.New("old request must remain failed")
	_, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	before, _ := m.Request("primary", r.ClientRequestID)
	calls := len(d.snapshotCalls())
	d.resumeErr = nil
	m.Coordination = &coordination.Store{StateDir: m.StateDir, Fault: func(stage string) error {
		if stage == "after-rename" {
			return errors.New("head parent sync interrupted")
		}
		return nil
	}}
	if _, err := m.StartRollback("primary", "20260916T130000Z-00000002"); err == nil {
		t.Fatal("uncertain host recovery dispatched")
	}
	if len(d.snapshotCalls()) != calls {
		t.Fatal("head failure had side effects")
	}
	restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
	if err := restart.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	if len(d.snapshotCalls()) != calls {
		t.Fatal("old request resumed after head failure")
	}
	after, _ := restart.Request("primary", r.ClientRequestID)
	if string(before.Operation) != string(after.Operation) {
		t.Fatal("old receipt changed")
	}
}

func TestNewAdmissionFollowsLegacyHeadWithoutUsingWallClock(t *testing.T) {
	m, d, auth, r := coordinationFixture(t, "backup")
	legacy, err := m.StartBackup("primary")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	earlier := m.now().Add(-time.Hour)
	m.Now = func() time.Time { return earlier }
	auth.counter = earlier.Unix() / 30
	plan, err := m.CreatePlan(context.Background(), "primary", coordination.PlanRequest{Action: "backup", ExpectedEpoch: r.ExpectedEpoch, Actor: r.Actor})
	if err != nil {
		t.Fatal(err)
	}
	r.PlanID = plan.PlanID
	r.PlanSHA256 = plan.PlanSHA256
	admitted, err := m.Submit(context.Background(), "primary", r, "123456", auth)
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(context.Background())
	calls := len(d.snapshotCalls())
	latest, err := m.coordinationStore().Latest("primary")
	if err != nil || latest.OperationID != admitted.OperationID || latest.Sequence != 2 || latest.Origin != "admission" {
		t.Fatal(latest, err)
	}
	if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
		t.Fatal(err)
	}
	restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
	if err := restart.Reconcile("primary"); err != nil {
		t.Fatal(err)
	}
	current, err := restart.Current("primary")
	if err != nil || current.ID != admitted.OperationID || current.ID == legacy.ID || len(d.snapshotCalls()) != calls {
		t.Fatal(current, err, d.snapshotCalls())
	}
}

func TestRepairSyncFailureCannotLeaveAStandaloneLegacyProjection(t *testing.T) {
	for _, loseUncommitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("power-loss-%v", loseUncommitted), func(t *testing.T) {
			m, d, auth, r := coordinationFixture(t, "backup")
			m.Coordination = &coordination.Store{StateDir: m.StateDir, Fault: func(stage string) error {
				if stage == "after-rename" {
					return errors.New("initial admission directory sync interrupted")
				}
				return nil
			}}
			receipt, err := m.Submit(context.Background(), "primary", r, "123456", auth)
			if err != nil || receipt.AdmissionStatus != "pending" {
				t.Fatal(receipt, err)
			}
			if err := os.RemoveAll(m.operationsDir("primary")); err != nil {
				t.Fatal(err)
			}
			restart := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now, Coordination: &coordination.Store{StateDir: m.StateDir, SyncDirectory: func(path string) error {
				if path == filepath.Join(m.StateDir, "coordination", "primary", "admissions") {
					return errors.New("retry admission directory sync failed")
				}
				return coordination.SyncDirectory(path)
			}}}
			if err := restart.Reconcile("primary"); err == nil {
				t.Fatal("directory failure ignored")
			}
			for _, path := range []string{restart.currentPath("primary"), filepath.Join(restart.operationsDir("primary"), receipt.OperationID+".json")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("projection preceded authority durability: %s %v", path, err)
				}
			}
			if loseUncommitted {
				if err := os.Remove(filepath.Join(m.StateDir, "coordination", "primary", "admissions", r.ClientRequestID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			clean := &Manager{StateDir: m.StateDir, Deployment: d, Now: m.Now}
			if err := clean.Reconcile("primary"); err != nil {
				t.Fatal(err)
			}
			if len(d.snapshotCalls()) != 0 {
				t.Fatal("undispatched request called deployment", d.snapshotCalls())
			}
		})
	}
}
