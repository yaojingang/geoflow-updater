package update_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type fakeDeployment struct {
	calls          []string
	failAt         string
	cancelAt       string
	cancel         context.CancelFunc
	rejectCanceled bool
}

func (deployment *fakeDeployment) call(ctx context.Context, name string) error {
	deployment.calls = append(deployment.calls, name)
	if deployment.cancelAt == name && deployment.cancel != nil {
		deployment.cancel()
	}
	if deployment.failAt == name {
		return errors.New(name + " failed")
	}
	if deployment.rejectCanceled && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (deployment *fakeDeployment) Resolve(context.Context, string) (managed.Release, error) {
	deployment.calls = append(deployment.calls, "resolve")
	return managed.Release{Sequence: 18, Version: "2.5.0"}, nil
}

func (deployment *fakeDeployment) Preflight(ctx context.Context, _ string, _ managed.Release) error {
	return deployment.call(ctx, "preflight")
}

func (deployment *fakeDeployment) Pull(ctx context.Context, _ string, _ managed.Release) error {
	return deployment.call(ctx, "pull")
}

func (deployment *fakeDeployment) Quiesce(ctx context.Context, _ string) error {
	return deployment.call(ctx, "quiesce")
}

func (deployment *fakeDeployment) CreateRecoveryPoint(ctx context.Context, _ string, _ string) (string, error) {
	if err := deployment.call(ctx, "backup"); err != nil {
		return "", err
	}
	return "rp-18", nil
}

func (deployment *fakeDeployment) Migrate(ctx context.Context, _ string, _ managed.Release) error {
	return deployment.call(ctx, "migrate")
}

func (deployment *fakeDeployment) Activate(ctx context.Context, _ string, _ managed.Release) error {
	return deployment.call(ctx, "activate")
}

func (deployment *fakeDeployment) Rollback(ctx context.Context, _ string, _ string) error {
	return deployment.call(ctx, "rollback")
}

func (deployment *fakeDeployment) Resume(ctx context.Context, _ string) error {
	return deployment.call(ctx, "resume")
}

func (deployment *fakeDeployment) Verify(ctx context.Context, _ string) error {
	return deployment.call(ctx, "verify")
}

func TestEngineCommitsOnlyAfterBackupMigrationActivationAndVerification(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	result := (update.Engine{Deployment: deployment}).Run(context.Background(), "primary", nil)

	if result.Status != update.StatusSucceeded || result.RecoveryPointID != "rp-18" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "migrate", "activate", "resume", "verify"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
}

func TestEngineAutomaticallyRollsBackAfterAProtectedStageFails(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{failAt: "migrate"}
	var stages []update.Stage
	result := (update.Engine{Deployment: deployment}).Run(context.Background(), "primary", func(stage update.Stage) error {
		stages = append(stages, stage)
		return nil
	})

	if result.Status != update.StatusRolledBack || result.RecoveryPointID != "rp-18" || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "migrate", "quiesce", "rollback", "resume", "verify"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
	if stages[len(stages)-1].Name != "rolled_back" {
		t.Fatalf("last stage = %#v", stages[len(stages)-1])
	}
}

func TestEngineRollsBackWhenRecoveryPointCannotBePersisted(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{}
	result := (update.Engine{Deployment: deployment}).Run(context.Background(), "primary", func(stage update.Stage) error {
		if stage.Name == "backup" && stage.Status == "succeeded" {
			return errors.New("disk full")
		}
		return nil
	})

	if result.Status != update.StatusRolledBack || result.RecoveryPointID != "rp-18" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "quiesce", "rollback", "resume", "verify"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
}

func TestEngineUsesAFreshContextForCompensationAfterTheOperationIsCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	deployment := &fakeDeployment{
		cancelAt:       "migrate",
		cancel:         cancel,
		rejectCanceled: true,
	}
	result := (update.Engine{Deployment: deployment}).Run(ctx, "primary", nil)

	if result.Status != update.StatusRolledBack {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "migrate", "quiesce", "rollback", "resume", "verify"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
}

func TestEngineRequiescesServicesBeforeRollbackAfterVerificationFails(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{failAt: "verify"}
	result := (update.Engine{Deployment: deployment}).Run(context.Background(), "primary", nil)

	if result.Status != update.StatusFailed || !strings.Contains(result.Error, "verify rolled back release") {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "migrate", "activate", "resume", "verify", "quiesce", "rollback", "resume", "verify"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
}

func TestEngineResumesTheCurrentReleaseWhenBackupCreationFails(t *testing.T) {
	t.Parallel()

	deployment := &fakeDeployment{failAt: "backup"}
	result := (update.Engine{Deployment: deployment}).Run(context.Background(), "primary", nil)

	if result.Status != update.StatusFailed || result.RecoveryPointID != "" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "resume"}
	if !reflect.DeepEqual(deployment.calls, want) {
		t.Fatalf("calls = %#v, want %#v", deployment.calls, want)
	}
}
