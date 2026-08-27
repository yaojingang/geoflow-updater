package update_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type fakeDeployment struct {
	calls  []string
	failAt string
}

func (deployment *fakeDeployment) call(name string) error {
	deployment.calls = append(deployment.calls, name)
	if deployment.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}

func (deployment *fakeDeployment) Resolve(context.Context, string) (managed.Release, error) {
	deployment.calls = append(deployment.calls, "resolve")
	return managed.Release{Sequence: 18, Version: "2.5.0"}, nil
}

func (deployment *fakeDeployment) Preflight(context.Context, string, managed.Release) error {
	return deployment.call("preflight")
}

func (deployment *fakeDeployment) Pull(context.Context, string, managed.Release) error {
	return deployment.call("pull")
}

func (deployment *fakeDeployment) Quiesce(context.Context, string) error {
	return deployment.call("quiesce")
}

func (deployment *fakeDeployment) CreateRecoveryPoint(context.Context, string, string) (string, error) {
	if err := deployment.call("backup"); err != nil {
		return "", err
	}
	return "rp-18", nil
}

func (deployment *fakeDeployment) Migrate(context.Context, string, managed.Release) error {
	return deployment.call("migrate")
}

func (deployment *fakeDeployment) Activate(context.Context, string, managed.Release) error {
	return deployment.call("activate")
}

func (deployment *fakeDeployment) Rollback(context.Context, string, string) error {
	return deployment.call("rollback")
}

func (deployment *fakeDeployment) Resume(context.Context, string) error {
	return deployment.call("resume")
}

func (deployment *fakeDeployment) Verify(context.Context, string) error {
	return deployment.call("verify")
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
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "migrate", "rollback", "resume", "verify"}
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
	want := []string{"resolve", "preflight", "pull", "quiesce", "backup", "rollback", "resume", "verify"}
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
