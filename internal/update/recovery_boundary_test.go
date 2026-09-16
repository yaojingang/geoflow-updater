package update_test

import (
	"context"
	"errors"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/update"
)

func TestRecoveryResumeStopsWhenBoundaryCannotBePersisted(t *testing.T) {
	for _, failure := range []string{"migrate", "quiesce", "backup", "persist-quiesce", "persist-backup"} {
		t.Run(failure, func(t *testing.T) {
			d := &fakeDeployment{failAt: failure}
			result := (update.Engine{Deployment: d}).Run(context.Background(), "primary", func(stage update.Stage) error {
				if stage.Name == "resume" && stage.Status == "running" {
					return errors.New("boundary storage unavailable")
				}
				if (failure == "persist-quiesce" && stage.Name == "quiesce" && stage.Status == "succeeded") ||
					(failure == "persist-backup" && stage.Name == "backup" && stage.Status == "running") {
					return errors.New("stage storage unavailable")
				}
				return nil
			})
			for _, call := range d.calls {
				if call == "resume" {
					t.Fatalf("opened services without boundary: %v", d.calls)
				}
			}
			if result.Status != update.StatusRecoveryRequired {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

type checkpointDeployment struct{ fakeDeployment }

func (d *checkpointDeployment) FreezeRecoveryCheckpoint(_ context.Context, _ string, request recovery.RestoreRequest) error {
	d.calls = append(d.calls, "freeze:"+request.TransactionID+":"+request.PointID)
	return nil
}
func TestAutomaticRecoveryFreezesCheckpointIdentityBeforeMigration(t *testing.T) {
	d := &checkpointDeployment{}
	result := (update.Engine{Deployment: d}).RunWithOptions(context.Background(), "primary", update.Options{AllowMaintenance: true, OperationID: "stable-upgrade-operation"}, nil)
	if result.Status != update.StatusSucceeded {
		t.Fatalf("result=%+v", result)
	}
	freeze, migrate := -1, -1
	for index, call := range d.calls {
		if strings.HasPrefix(call, "freeze:stable-upgrade-operation:") {
			freeze = index
		}
		if call == "migrate" {
			migrate = index
		}
	}
	if freeze < 0 || migrate <= freeze {
		t.Fatalf("migration started without a frozen administrator checkpoint: %v", d.calls)
	}
}

type observedResumeDeployment struct {
	fakeDeployment
	beforeResume func()
}

func (d *observedResumeDeployment) Resume(ctx context.Context, id string) error {
	d.beforeResume()
	return d.fakeDeployment.Resume(ctx, id)
}

func TestRecoveryPersistsResumeBeforeOpeningServices(t *testing.T) {
	for _, failure := range []string{"", "migrate", "quiesce", "backup", "persist-quiesce", "persist-backup"} {
		t.Run(failure, func(t *testing.T) {
			var persisted update.Stage
			opened := 0
			d := &observedResumeDeployment{fakeDeployment: fakeDeployment{failAt: failure}}
			d.beforeResume = func() {
				opened++
				if persisted.Name != "resume" || persisted.Status != "running" || persisted.Message != "service resume boundary v1" {
					t.Fatalf("opened services with stage %+v", persisted)
				}
			}
			result := (update.Engine{Deployment: d}).Run(context.Background(), "primary", func(stage update.Stage) error {
				if (failure == "persist-quiesce" && stage.Name == "quiesce" && stage.Status == "succeeded") ||
					(failure == "persist-backup" && stage.Name == "backup" && stage.Status == "running") {
					return errors.New("stage storage unavailable")
				}
				persisted = stage
				return nil
			})
			expected := update.StatusFailed
			if failure == "" {
				expected = update.StatusSucceeded
			} else if failure == "migrate" {
				expected = update.StatusRolledBack
			}
			if opened != 1 || result.Status != expected {
				t.Fatalf("opened=%d status=%s error=%s", opened, result.Status, result.Error)
			}
		})
	}
}
