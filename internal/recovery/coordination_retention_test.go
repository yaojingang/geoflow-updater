package recovery_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/coordination"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
)

func TestRetentionProtectsPlansUnresolvedAdmissionsAndLatestCheckpoint(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	instanceDir := filepath.Join(state, "instances", "primary")
	for path, data := range map[string]string{filepath.Join(root, ".env.prod"): "APP_ENV=production", filepath.Join(root, "version.json"): "{}", filepath.Join(instanceDir, "instance.yml"): "instance", filepath.Join(instanceDir, "release.env"): "release", filepath.Join(instanceDir, "docker-compose.yml"): "compose"} {
		mustWrite(t, path, []byte(data), 0600)
	}
	for _, path := range []string{filepath.Join(root, "storage"), filepath.Join(root, "docker-data", "prod", "redis")} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(instanceDir, "docker-compose.yml"), EnvironmentFile: filepath.Join(instanceDir, "release.env"), Version: "3.1.0", ReleaseSequence: 1}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := recovery.Store{Control: &recoverycontrol.Store{StateDir: state}, BackupRoot: t.TempDir(), Keep: 1, Now: func() time.Time { now = now.Add(time.Second); return now }}
	journal := coordination.Store{StateDir: state}
	create := func(reason string) recovery.Point {
		t.Helper()
		point, err := store.Create(context.Background(), config, reason, &database{dump: []byte("db")})
		if err != nil {
			t.Fatal(err)
		}
		return point
	}
	point := create("update-to-3.2.0")
	actor := coordination.Actor{ManagementInstanceID: "12345678-1234-4234-8234-123456789012", AdminID: 1, IdentitySHA256: strings.Repeat("a", 64)}
	plan := coordination.Plan{SchemaVersion: 2, InstanceID: "primary", PlanID: strings.Repeat("b", 32), ExpectedEpoch: strings.Repeat("c", 32), Action: "restore", Actor: actor, ExpiresAt: now.Add(10 * time.Minute), BaselineSHA256: strings.Repeat("d", 64), Continuation: "host_only", RecoveryPointID: point.ID}
	plan.PlanSHA256 = plan.Digest()
	if err := journal.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	latest := create("update-to-3.3.0")
	create("manual")
	points, err := store.List("primary")
	if err != nil || len(points) != 3 {
		t.Fatal("valid plan and latest checkpoint not protected beyond keep", points, err)
	}
	admission := coordination.Admission{SchemaVersion: 2, InstanceID: "primary", ClientRequestID: "retention-request", BusinessSHA256: strings.Repeat("e", 64), OperationID: "20260916T120000.000000000Z-0000000000000001", Action: "restore", PlanID: plan.PlanID, AcceptedEpoch: plan.ExpectedEpoch, Actor: actor, Scope: "rollback", Counter: now.Unix() / 30, AcceptedAt: now, RecoveryPointID: point.ID, Operation: json.RawMessage(`{"status":"recovery_required"}`)}
	if err := journal.Accept(admission); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	create("manual")
	points, err = store.List("primary")
	if err != nil || len(points) != 3 {
		t.Fatal("unresolved admission not retained after plan expiry", points, err)
	}
	if err := journal.UpdateOperation("primary", admission.OperationID, json.RawMessage(`{"status":"succeeded"}`)); err != nil {
		t.Fatal(err)
	}
	create("manual")
	points, err = store.List("primary")
	if err != nil || len(points) != 2 {
		t.Fatal(points, err)
	}
	for _, p := range points {
		if p.ID == point.ID {
			t.Fatal("expired released reference kept unexpectedly")
		}
	}
	found := false
	for _, p := range points {
		found = found || p.ID == latest.ID
	}
	if !found {
		t.Fatal("latest checkpoint was pruned")
	}
}
