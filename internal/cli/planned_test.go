package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/cli"
	"github.com/yaojingang/geoflow-updater/internal/deployment"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

type plannedController struct {
	previews, mutations int
	options             update.Options
	err                 error
}

func (f *plannedController) Preview(context.Context, string) (update.PlanSummary, error) {
	f.previews++
	return update.PlanSummary{SchemaVersion: 1, TargetVersion: "3.0.0", Strategy: "maintenance", PlanSHA256: strings.Repeat("a", 64)}, f.err
}
func (f *plannedController) StartUpdate(string) (operation.Operation, error) {
	return operation.Operation{}, errors.New("unexpected legacy fallback")
}
func (f *plannedController) StartUpdateWithOptions(_ string, o update.Options) (operation.Operation, error) {
	f.options = o
	f.mutations++
	return operation.Operation{ID: "update"}, f.err
}
func (f *plannedController) StartSwitchBack(string) (operation.Operation, error) {
	f.mutations++
	return operation.Operation{ID: "switch-back"}, f.err
}
func (f *plannedController) StartBackup(string) (operation.Operation, error) {
	return operation.Operation{}, nil
}
func (f *plannedController) StartRollback(string, string) (operation.Operation, error) {
	return operation.Operation{}, nil
}
func (f *plannedController) StartVerify(string) (operation.Operation, error) {
	return operation.Operation{}, nil
}
func (f *plannedController) Current(string) (operation.Operation, error) {
	return operation.Operation{}, nil
}
func (f *plannedController) Get(_ string, id string) (operation.Operation, error) {
	now := time.Now()
	return operation.Operation{ID: id, Status: operation.StatusSucceeded, CompletedAt: &now}, nil
}
func (f *plannedController) RecoveryPoints(string) ([]recovery.Point, error) { return nil, nil }

func TestPlannedCLISeparatesPreviewConfirmationAndSwitchBack(t *testing.T) {
	f := &plannedController{}
	out := &bytes.Buffer{}
	errout := &bytes.Buffer{}
	app := cli.App{Operations: f, Stdout: out, Stderr: errout}
	if code := app.Run(context.Background(), []string{"update", "--dry-run", "--json"}); code != 0 || f.previews != 1 || f.mutations != 0 || !strings.Contains(out.String(), `"plan_sha256"`) {
		t.Fatalf("preview: %d %s %s %#v", code, out, errout, f)
	}
	if code := app.Run(context.Background(), []string{"update", "--allow-maintenance", "--plan-sha256", strings.Repeat("a", 64)}); code != 0 || !f.options.AllowMaintenance || f.options.ExpectedPlanSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("confirmed update: %d %#v", code, f)
	}
	if code := app.Run(context.Background(), []string{"update"}); code != 0 || f.options.AllowMaintenance {
		t.Fatalf("implicit maintenance authorization: %d %#v", code, f)
	}
	if code := app.Run(context.Background(), []string{"switch-back"}); code != 0 || f.mutations != 3 {
		t.Fatalf("switch-back: %d %#v", code, f)
	}
}
func TestPlannedCLIRejectsMalformedOptionsAndDoesNotFallbackAfterPreviewFailure(t *testing.T) {
	for _, args := range [][]string{{"update", "--plan-sha256", "bad"}, {"backup", "--allow-maintenance"}, {"update", "--dry-run", "--allow-maintenance"}, {"switch-back", "--recovery-point", "data"}} {
		f := &plannedController{}
		if code := (cli.App{Operations: f}).Run(context.Background(), args); code != 2 || f.mutations != 0 {
			t.Fatalf("%v: %d %#v", args, code, f)
		}
	}
	f := &plannedController{err: errors.New("plan hash mismatch")}
	if code := (cli.App{Operations: f}).Run(context.Background(), []string{"update", "--dry-run"}); code != 1 || f.mutations != 0 {
		t.Fatalf("failed preview: %d %#v", code, f)
	}
}

type installer struct{ request deployment.InstallRequest }

func (f *installer) Install(_ context.Context, r deployment.InstallRequest) (deployment.InstallResult, error) {
	f.request = r
	return deployment.InstallResult{Instance: instance.Config{ID: r.InstanceID, Root: r.Root}, CredentialsFile: "/private/credentials"}, nil
}
func TestInstallCLIUsesBoundedDeploymentRequest(t *testing.T) {
	f := &installer{}
	out := &bytes.Buffer{}
	code := (cli.App{Installer: f, Stdout: out}).Run(context.Background(), []string{"install", "--instance", "primary", "--root", "/opt/geoflow", "--url", "https://geo.example"})
	if code != 0 || f.request.URL != "https://geo.example" || !strings.Contains(out.String(), "/private/credentials") {
		t.Fatalf("install: %d %#v %s", code, f, out)
	}
}
