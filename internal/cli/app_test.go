package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/cli"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/instance"
)

type enroller struct {
	request enrollment.Request
}

func (fake *enroller) Enroll(_ context.Context, request enrollment.Request) (enrollment.Result, error) {
	fake.request = request
	return enrollment.Result{Instance: instance.Config{
		Root:            "/opt/geoflow",
		EnvironmentFile: "/var/lib/geoflow-updater/instances/primary/release.env",
		ComposeFile:     "/var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml",
	}}, nil
}

type diagnostician struct {
	report doctor.Report
}

func (fake diagnostician) Run(context.Context, string) doctor.Report {
	return fake.report
}

func TestEnrollCommandUsesFixedCommandSurface(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	fakeEnroller := &enroller{}
	exitCode := (cli.App{Stdout: stdout, Stderr: &bytes.Buffer{}, Enroller: fakeEnroller}).Run(
		context.Background(),
		[]string{"enroll", "--instance-id", "primary", "--instance-root", "/opt/geoflow"},
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if fakeEnroller.request.InstanceID != "primary" || fakeEnroller.request.Root != "/opt/geoflow" {
		t.Fatalf("enrollment request = %#v", fakeEnroller.request)
	}
	if !strings.Contains(stdout.String(), "primary") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "confirm every GEOFlow queue is idle") {
		t.Fatalf("stdout does not include the queue drain warning: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "docker compose --env-file /opt/geoflow/.env.prod --env-file /var/lib/geoflow-updater/instances/primary/release.env -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml down") {
		t.Fatalf("stdout does not include the safe legacy handover command: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "docker compose --env-file /opt/geoflow/.env.prod --env-file /var/lib/geoflow-updater/instances/primary/release.env -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml up -d") {
		t.Fatalf("stdout does not include the managed deployment command with both environment files: %q", stdout.String())
	}
}

func TestDoctorJSONReturnsNonZeroForFailedReport(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	exitCode := (cli.App{
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		Doctor: diagnostician{report: doctor.Report{
			SchemaVersion: 1,
			Status:        doctor.StatusFail,
			Checks:        []doctor.Check{{ID: "platform", Status: doctor.StatusFail, Message: "Linux required"}},
		}},
	}).Run(context.Background(), []string{"doctor", "--instance", "primary", "--json"})

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stdout.String(), `"status": "fail"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUnknownCommandDoesNotExposeAnArbitraryExecutionHook(t *testing.T) {
	t.Parallel()

	stderr := &bytes.Buffer{}
	exitCode := (cli.App{Stdout: &bytes.Buffer{}, Stderr: stderr}).Run(context.Background(), []string{"exec", "rm", "-rf", "/"})

	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
