package authorization_test

import (
	"encoding/json"
	"errors"
	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdmissionAuthorityProtectsOTPWhenCounterProjectionIsLost(t *testing.T) {
	state := t.TempDir()
	writeEnrollment(t, state)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := authorization.Service{StateDir: state, Now: func() time.Time { return now }}
	f, err := s.Provision("primary")
	if err != nil {
		t.Fatal(err)
	}
	code := codeFor(t, f, authorization.ScopeUpdate, now)
	called := 0
	err = s.AuthorizeAdmission("primary", authorization.ScopeUpdate, code, nil, func(counter int64) error {
		called++
		return (coordination.Store{StateDir: state}).Accept(coordination.Admission{SchemaVersion: 2, InstanceID: "primary", ClientRequestID: "request-0001", BusinessSHA256: strings.Repeat("a", 64), OperationID: "20260916T120000.000000000Z-0000000000000001", Action: "update", PlanID: strings.Repeat("b", 32), AcceptedEpoch: strings.Repeat("c", 32), Actor: coordination.Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("d", 64)}, Scope: "update", Counter: counter, AcceptedAt: now, Operation: json.RawMessage(`{"status":"queued"}`)})
	})
	if err != nil || called != 1 {
		t.Fatal(called, err)
	}
	if err := os.Remove(filepath.Join(state, "instances", "primary", "mutation.update.counter")); err != nil {
		t.Fatal(err)
	}
	for _, v2 := range []bool{false, true} {
		var err error
		if v2 {
			err = s.AuthorizeAdmission("primary", authorization.ScopeUpdate, code, nil, func(int64) error { t.Fatal("replay accepted"); return nil })
		} else {
			err = s.Authorize("primary", authorization.ScopeUpdate, code, func() error { t.Fatal("v1 replay accepted"); return nil })
		}
		if !errors.Is(err, authorization.ErrReplay) {
			t.Fatalf("v2=%v err=%v", v2, err)
		}
	}
	if err := s.AuthorizeAdmission("primary", authorization.ScopeUpdate, "", func() (bool, error) { return true, nil }, func(int64) error { t.Fatal("duplicate dispatched"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyConsumptionAlsoBlocksAdmission(t *testing.T) {
	state := t.TempDir()
	writeEnrollment(t, state)
	now := time.Now()
	s := authorization.Service{StateDir: state, Now: func() time.Time { return now }}
	f, _ := s.Provision("primary")
	code := codeFor(t, f, authorization.ScopeBackup, now)
	if err := s.Authorize("primary", authorization.ScopeBackup, code, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeAdmission("primary", authorization.ScopeBackup, code, nil, func(int64) error { t.Fatal("legacy replay accepted"); return nil }); !errors.Is(err, authorization.ErrReplay) {
		t.Fatal(err)
	}
}

func TestAdmissionCleanupFailureRemainsAcceptedAndQueryable(t *testing.T) {
	state := t.TempDir()
	writeEnrollment(t, state)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := authorization.Service{StateDir: state, Now: func() time.Time { return now }}
	f, _ := s.Provision("primary")
	code := codeFor(t, f, authorization.ScopeBackup, now)
	accepted := false
	err := s.AuthorizeAdmission("primary", authorization.ScopeBackup, code, nil, func(counter int64) error {
		record := coordination.Admission{SchemaVersion: 2, InstanceID: "primary", ClientRequestID: "request-cleanup", BusinessSHA256: strings.Repeat("a", 64), OperationID: "20260916T120000.000000000Z-0000000000000001", Action: "backup", PlanID: strings.Repeat("b", 32), AcceptedEpoch: strings.Repeat("c", 32), Actor: coordination.Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("d", 64)}, Scope: "backup", Counter: counter, AcceptedAt: now, Operation: json.RawMessage(`{"status":"queued"}`)}
		if err := (coordination.Store{StateDir: state}).Accept(record); err != nil {
			return err
		}
		accepted = true
		for _, suffix := range []string{"counter", "attempts"} {
			if err := os.Mkdir(filepath.Join(state, "instances", "primary", "mutation.backup."+suffix), 0700); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || !accepted {
		t.Fatalf("accepted action converted to failure: %v", err)
	}
	if err := s.AuthorizeAdmission("primary", authorization.ScopeBackup, "", func() (bool, error) {
		_, err := (coordination.Store{StateDir: state}).Request("primary", "request-cleanup")
		return err == nil, err
	}, func(int64) error { t.Fatal("repeated action"); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"counter", "attempts"} {
		if err := os.Remove(filepath.Join(state, "instances", "primary", "mutation.backup."+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Authorize("primary", authorization.ScopeBackup, code, func() error { t.Fatal("accepted code was reusable"); return nil }); !errors.Is(err, authorization.ErrReplay) {
		t.Fatal(err)
	}
}

func TestAdmissionAndLegacyShareAttemptLockout(t *testing.T) {
	state := t.TempDir()
	writeEnrollment(t, state)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := authorization.Service{StateDir: state, Now: func() time.Time { return now }}
	f, _ := s.Provision("primary")
	code := codeFor(t, f, authorization.ScopeBackup, now)
	invalid := "000000"
	if code == invalid {
		invalid = "000001"
	}
	for i := 0; i < 5; i++ {
		var err error
		if i%2 == 0 {
			err = s.Authorize("primary", authorization.ScopeBackup, invalid, func() error { t.Fatal("bad factor accepted"); return nil })
		} else {
			err = s.AuthorizeAdmission("primary", authorization.ScopeBackup, invalid, nil, func(int64) error { t.Fatal("bad factor accepted"); return nil })
		}
		want := authorization.ErrInvalid
		if i == 4 {
			want = authorization.ErrRateLimited
		}
		if !errors.Is(err, want) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := s.AuthorizeAdmission("primary", authorization.ScopeBackup, code, nil, func(int64) error { t.Fatal("locked factor accepted"); return nil }); !errors.Is(err, authorization.ErrRateLimited) {
		t.Fatal(err)
	}
	if retry := s.RetryAfter("primary", authorization.ScopeBackup); retry != 900 {
		t.Fatal(retry)
	}
}
