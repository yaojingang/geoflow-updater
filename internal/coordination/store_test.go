package coordination

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureAdmission() Admission {
	return Admission{SchemaVersion: 2, InstanceID: "primary", ClientRequestID: "request-0001", BusinessSHA256: strings.Repeat("a", 64), OperationID: "20260916T120000.000000000Z-0000000000000001", Action: "restore", PlanID: strings.Repeat("b", 32), AcceptedEpoch: strings.Repeat("c", 32), Scope: "rollback", Counter: 123, Actor: Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("d", 64)}, AcceptedAt: time.Now().UTC(), RecoveryPointID: "20260916T120000Z-00000001", Operation: json.RawMessage(`{"schema_version":1,"status":"queued"}`)}
}
func TestAdmissionIsAuthorityAndCannotBeRebound(t *testing.T) {
	s := Store{StateDir: t.TempDir()}
	a := fixtureAdmission()
	if err := s.Accept(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Request("primary", a.ClientRequestID)
	if err != nil || got.OperationID != a.OperationID {
		t.Fatalf("read: %#v %v", got, err)
	}
	if err := s.Accept(a); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite: %v", err)
	}
	count, err := s.Counter("primary", "rollback")
	if err != nil || count != 123 {
		t.Fatalf("counter: %d %v", count, err)
	}
	got.Operation = json.RawMessage(`{"schema_version":1,"status":"succeeded"}`)
	if err := s.UpdateOperation("primary", a.OperationID, got.Operation); err != nil {
		t.Fatal(err)
	}
	got, err = s.Operation("primary", a.OperationID)
	if err != nil || !strings.Contains(string(got.Operation), "succeeded") {
		t.Fatal(got, err)
	}
	if _, err := s.Request("primary", "../escape"); err == nil {
		t.Fatal("unsafe request accepted")
	}
}
func TestAdmissionDurabilityFailuresNeverLoseVisibleIdentity(t *testing.T) {
	for _, point := range []string{"before-write", "before-rename", "after-rename", "after-sync"} {
		t.Run(point, func(t *testing.T) {
			s := Store{StateDir: t.TempDir(), Fault: func(stage string) error {
				if stage == point {
					return errors.New("crash")
				}
				return nil
			}}
			a := fixtureAdmission()
			if err := s.Accept(a); err == nil {
				t.Fatal("expected failure")
			}
			got, err := s.Request("primary", a.ClientRequestID)
			if point == "after-rename" || point == "after-sync" {
				if err != nil || got.OperationID != a.OperationID {
					t.Fatalf("lost identity: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected record: %v", err)
			}
		})
	}
}
func TestPlanDigestBindsPrivateDetailsAndProtectedReferences(t *testing.T) {
	s := Store{StateDir: t.TempDir()}
	a := fixtureAdmission()
	now := time.Now().UTC()
	p := Plan{SchemaVersion: 2, InstanceID: "primary", PlanID: a.PlanID, ExpectedEpoch: a.AcceptedEpoch, Action: "restore", Actor: a.Actor, ExpiresAt: now.Add(time.Minute), BaselineSHA256: strings.Repeat("e", 64), Continuation: "host_only", RecoveryPointID: a.RecoveryPointID, Snapshot: json.RawMessage(`{"manifest":"old"}`)}
	p.PlanSHA256 = p.Digest()
	if err := s.SavePlan(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Plan("primary", p.PlanID)
	if err != nil || got.PlanSHA256 != p.PlanSHA256 {
		t.Fatal(got, err)
	}
	refs, err := s.ProtectedPoints("primary", now)
	if err != nil || !refs[a.RecoveryPointID] {
		t.Fatal(refs, err)
	}
	refs, err = s.ProtectedPoints("primary", now.Add(time.Hour))
	if err != nil || refs[a.RecoveryPointID] {
		t.Fatal(refs, err)
	}
	if err := s.Accept(a); err != nil {
		t.Fatal(err)
	}
	refs, err = s.ProtectedPoints("primary", now.Add(time.Hour))
	if err != nil || !refs[a.RecoveryPointID] {
		t.Fatal(refs, err)
	}
	files, _ := filepath.Glob(filepath.Join(s.StateDir, "coordination", "primary", "plans", "*.json"))
	contents, _ := os.ReadFile(files[0])
	contents = []byte(strings.Replace(string(contents), "old", "new", 1))
	if err := os.WriteFile(files[0], contents, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan("primary", p.PlanID); err == nil {
		t.Fatal("modified private plan accepted")
	}
}

func TestDirectorySyncFailuresRemainBlockingAcrossRetries(t *testing.T) {
	for _, relative := range []string{"coordination", "coordination/primary", "coordination/primary/admissions"} {
		t.Run(relative, func(t *testing.T) {
			state := t.TempDir()
			child := filepath.Join(state, relative)
			parent := filepath.Dir(child)
			blocked := true
			failures := 0
			syncs := []string{}
			s := Store{StateDir: state, SyncDirectory: func(path string) error {
				syncs = append(syncs, path)
				if path == parent {
					if _, err := os.Stat(child); err == nil && blocked {
						failures++
						return errors.New("directory sync failed")
					}
				}
				return SyncDirectory(path)
			}}
			a := fixtureAdmission()
			for attempt := 0; attempt < 2; attempt++ {
				if err := s.Accept(a); err == nil {
					t.Fatalf("retry %d accepted without durable %s entry", attempt, child)
				}
				if _, err := s.Request("primary", a.ClientRequestID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unsafe admission became visible", err)
				}
			}
			if failures != 2 {
				t.Fatal("failed ancestor was skipped on retry", failures, syncs)
			}
			blocked = false
			if err := s.Accept(a); err != nil {
				t.Fatal(err)
			}
			record, err := s.Request("primary", a.ClientRequestID)
			if err != nil || record.OperationID != a.OperationID {
				t.Fatal(record, err)
			}
		})
	}
}
