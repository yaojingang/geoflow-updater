package deployment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

func TestActionSnapshotsBindCurrentConfigAndTrustedTarget(t *testing.T) {
	service, release, config := enrolledConversionFixture(t)
	if _, err := (recoverycontrol.Store{StateDir: service.StateDir}).Initialize("primary"); err != nil {
		t.Fatal(err)
	}
	first, err := service.ActionSnapshot(context.Background(), "primary", "update", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Target == nil || first.Target.AppImage != release.AppImage || first.UpdatePlanSHA256 == "" || first.Continuation != "host_only" || !first.MaintenanceRequired {
		t.Fatal(first)
	}
	second, err := service.ActionSnapshot(context.Background(), "primary", "update", "")
	if err != nil || first.BaselineSHA256 != second.BaselineSHA256 {
		t.Fatal(second, err)
	}
	backup, err := service.ActionSnapshot(context.Background(), "primary", "backup", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.Root, ".env.prod")
	contents, _ := os.ReadFile(path)
	writeTest(t, path, append(contents, []byte("# configuration changed\n")...))
	changed, err := service.ActionSnapshot(context.Background(), "primary", "backup", "")
	if err != nil || backup.BaselineSHA256 == changed.BaselineSHA256 {
		t.Fatal("backup did not bind site configuration", err)
	}
	_, err = (recoverycontrol.Store{StateDir: service.StateDir}).Begin("primary", "20260916T120000Z-00000001", "other-transaction", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ActionSnapshot(context.Background(), "primary", "backup", ""); err == nil {
		t.Fatal("held background admitted backup")
	}
}

type planRecoveryStore struct {
	recordingRecoveryStore
	points []recovery.Point
}

func (p *planRecoveryStore) List(string) ([]recovery.Point, error) { return p.points, nil }
func TestRestoreSnapshotOnlyPermitsLatestCheckpointAndBindsManifest(t *testing.T) {
	service, _, _ := enrolledConversionFixture(t)
	if _, err := (recoverycontrol.Store{StateDir: service.StateDir}).Initialize("primary"); err != nil {
		t.Fatal(err)
	}
	points := []recovery.Point{{ID: "20260916T140000Z-00000003", Reason: "manual", CreatedAt: time.Now()}, {ID: "20260916T130000Z-00000002", Reason: "update-to-3.2.0", Version: "3.1.0", CreatedAt: time.Now(), Files: map[string]recovery.FileRecord{"database.dump": {SHA256: strings.Repeat("a", 64)}}}, {ID: "20260916T120000Z-00000001", Reason: "update-to-3.1.0", CreatedAt: time.Now()}}
	store := &planRecoveryStore{points: points}
	service.Recoveries = store
	for _, id := range []string{points[0].ID, points[2].ID} {
		if _, err := service.ActionSnapshot(context.Background(), "primary", "restore", id); err == nil {
			t.Fatal("accepted non-latest point", id)
		}
	}
	first, err := service.ActionSnapshot(context.Background(), "primary", "restore", points[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	store.points[1].Files["database.dump"] = recovery.FileRecord{SHA256: strings.Repeat("b", 64)}
	second, err := service.ActionSnapshot(context.Background(), "primary", "restore", points[1].ID)
	if err != nil || first.BaselineSHA256 == second.BaselineSHA256 {
		t.Fatal("manifest changed without baseline change", err)
	}
}
func TestSwitchBackSnapshotBindsOriginalTransactionAndRetainedFiles(t *testing.T) {
	service, _, candidate, tx := recoveryTopologyFixture(t)
	_, err := (recoverycontrol.Store{StateDir: service.StateDir}).Initialize("primary")
	if err != nil {
		t.Fatal(err)
	}
	tx.Source = candidate
	tx.Source.ActiveSlot = "green"
	tx.Source.ComposeFile = filepath.Join(service.instanceDirectory("primary"), "slots", "green", "docker-compose.yml")
	tx.Source.EnvironmentFile = filepath.Join(filepath.Dir(tx.Source.ComposeFile), "release.env")
	tx.Source.ReleaseSequence = 7
	tx.Strategy = managed.StrategyOnline
	tx.Status = update.StatusSucceeded
	tx.Target.Sequence = candidate.ReleaseSequence
	tx.Target.UpgradePlan = nil
	tx.Target.MinimumUpdaterProtocol = 1
	for path, data := range map[string][]byte{tx.Source.ComposeFile: []byte("retained compose"), tx.Source.EnvironmentFile: []byte("retained env"), filepath.Join(filepath.Dir(candidate.InfraComposeFile), "traffic", "nginx.conf"): []byte("active traffic"), filepath.Join(filepath.Dir(candidate.ComposeFile), "upgrade-plan.json"): []byte("{}"), filepath.Join(filepath.Dir(candidate.ComposeFile), "version.json"): []byte("{}")} {
		writeTest(t, path, data)
	}
	config, _ := yaml.Marshal(candidate)
	writeTest(t, filepath.Join(service.instanceDirectory("primary"), "instance.yml"), config)
	if err := service.saveTransaction(&tx); err != nil {
		t.Fatal(err)
	}
	first, err := service.ActionSnapshot(context.Background(), "primary", "switch-back", "")
	if err != nil {
		t.Fatal(err)
	}
	tx.OperationID = "different-original-transaction"
	if err := service.saveTransaction(&tx); err != nil {
		t.Fatal(err)
	}
	second, err := service.ActionSnapshot(context.Background(), "primary", "switch-back", "")
	if err != nil || first.BaselineSHA256 == second.BaselineSHA256 {
		t.Fatal("transaction was not bound", err)
	}
	writeTest(t, tx.Source.EnvironmentFile, []byte("retained env changed"))
	third, err := service.ActionSnapshot(context.Background(), "primary", "switch-back", "")
	if err != nil || second.BaselineSHA256 == third.BaselineSHA256 {
		t.Fatal("retained config was not bound", err)
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(first.Details, &details); err != nil || details["target"] == nil {
		t.Fatal(details, err)
	}
}
