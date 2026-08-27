package doctor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
)

type probe struct {
	operatingSystem string
	commands        map[string]error
	inspectOutput   string
	forbiddenOutput string
}

func (fake probe) OperatingSystem() string {
	return fake.operatingSystem
}

func (fake probe) CommandOutput(_ context.Context, _ string, arguments ...string) (string, error) {
	if len(arguments) > 0 && arguments[0] == "inspect" {
		return fake.inspectOutput, fake.commands["docker-inspect"]
	}
	if len(arguments) > 0 && arguments[0] == "ps" {
		return fake.forbiddenOutput, fake.commands["docker-forbidden"]
	}
	if len(arguments) > 0 && arguments[len(arguments)-1] == "--quiet" {
		return "", fake.commands["managed-compose"]
	}

	return "", fake.commands["docker-compose"]
}

func TestDoctorPassesForHealthyLinuxManagedInstance(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), "APP_ENV=production\n", 0o600)
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`, 0o600)
	mustMkdir(t, filepath.Join(root, "storage"))
	postgresDataDir := filepath.Join(root, "docker-data", "prod", "postgres")
	mustMkdir(t, filepath.Join(postgresDataDir, "18", "docker"))
	mustWriteFile(t, filepath.Join(postgresDataDir, "18", "docker", "PG_VERSION"), "18\n", 0o600)

	instanceDir := filepath.Join(stateDir, "instances", "primary")
	mustMkdir(t, instanceDir)
	mustWriteFile(t, filepath.Join(instanceDir, "docker-compose.managed.yml"), "services: {}\n", 0o640)
	mustWriteFile(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_COMPOSE_PROJECT_NAME=geoflow-laravel-prod\nGEOFLOW_INSTANCE_ID=primary\nGEOFLOW_INSTANCE_ROOT=\""+root+"\"\nGEOFLOW_RELEASE_SEQUENCE=17\nGEOFLOW_VERSION=2.4.0\nGEOFLOW_UPDATER_STATE_DIR=\""+stateDir+"\"\nGEOFLOW_UPDATER_GROUP_ID=991\nGEOFLOW_APP_IMAGE=ghcr.io/yaojingang/geoflow-app@sha256:"+strings.Repeat("1", 64)+"\nGEOFLOW_WEB_IMAGE=ghcr.io/yaojingang/geoflow-web@sha256:"+strings.Repeat("2", 64)+"\nGEOFLOW_POSTGRES_IMAGE=pgvector/pgvector@sha256:"+strings.Repeat("3", 64)+"\nGEOFLOW_POSTGRES_DATA_DIR=\""+postgresDataDir+"\"\nGEOFLOW_POSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql\nGEOFLOW_REDIS_IMAGE=redis@sha256:"+strings.Repeat("4", 64)+"\n", 0o640)
	mustWriteFile(t, filepath.Join(instanceDir, "control.token"), strings.Repeat("a", 43)+"\n", 0o640)
	mustWriteFile(t, filepath.Join(instanceDir, "mutation.secret"), strings.Repeat("A", 32)+"\n", 0o600)
	mustWriteFile(t, filepath.Join(instanceDir, "instance.yml"), "schema_version: 1\nid: primary\nroot: "+root+"\ncompose_file: "+filepath.Join(instanceDir, "docker-compose.managed.yml")+"\nenvironment_file: "+filepath.Join(instanceDir, "release.env")+"\ncontrol_token_file: "+filepath.Join(instanceDir, "control.token")+"\nrelease_sequence: 17\nversion: 2.4.0\npostgres_major: 18\npostgres_data_dir: "+postgresDataDir+"\npostgres_container_data_dir: /var/lib/postgresql\nredis_major: 8\nenrolled_at: 2026-08-27T09:30:00Z\n", 0o640)

	report := doctor.Service{
		StateDir: stateDir,
		Probe: probe{
			operatingSystem: "linux",
			commands:        map[string]error{},
			inspectOutput: strings.Join([]string{
				"/geoflow-postgres-prod|running|healthy",
				"/geoflow-redis-prod|running|healthy",
				"/geoflow-app-prod|running|healthy",
				"/geoflow-web-prod|running|healthy",
				"/geoflow-queue-prod|running|none",
				"/geoflow-knowledge-queue-prod|running|none",
				"/geoflow-scheduler-prod|running|none",
				"/geoflow-reverb-prod|running|none",
			}, "\n"),
		},
	}.Run(context.Background(), "primary")

	if report.Status != doctor.StatusPass {
		t.Fatalf("doctor status = %q, want pass; checks = %#v", report.Status, report.Checks)
	}
	if report.Instance == nil || report.Instance.ReleaseSequence != 17 {
		t.Fatalf("doctor instance = %#v, want release sequence 17", report.Instance)
	}
	assertCheck(t, report, "platform", doctor.StatusPass)
	assertCheck(t, report, "docker-compose", doctor.StatusPass)
	assertCheck(t, report, "installed-version", doctor.StatusPass)
	assertCheck(t, report, "release-pins", doctor.StatusPass)
	assertCheck(t, report, "mutation-authorization", doctor.StatusPass)
	assertCheck(t, report, "managed-deployment", doctor.StatusPass)

	orphanReport := doctor.Service{
		StateDir: stateDir,
		Probe: probe{
			operatingSystem: "linux",
			commands:        map[string]error{},
			inspectOutput: strings.Join([]string{
				"/geoflow-postgres-prod|running|healthy",
				"/geoflow-redis-prod|running|healthy",
				"/geoflow-app-prod|running|healthy",
				"/geoflow-web-prod|running|healthy",
				"/geoflow-queue-prod|running|none",
				"/geoflow-knowledge-queue-prod|running|none",
				"/geoflow-scheduler-prod|running|none",
				"/geoflow-reverb-prod|running|none",
			}, "\n"),
			forbiddenOutput: "geoflow-system-update-queue-prod\n",
		},
	}.Run(context.Background(), "primary")
	assertCheck(t, orphanReport, "retired-update-worker", doctor.StatusFail)
	assertCheck(t, orphanReport, "managed-deployment", doctor.StatusPass)

	inspectionFailureReport := doctor.Service{
		StateDir: stateDir,
		Probe: probe{
			operatingSystem: "linux",
			commands:        map[string]error{"docker-forbidden": errors.New("permission denied")},
			inspectOutput: strings.Join([]string{
				"/geoflow-postgres-prod|running|healthy",
				"/geoflow-redis-prod|running|healthy",
				"/geoflow-app-prod|running|healthy",
				"/geoflow-web-prod|running|healthy",
				"/geoflow-queue-prod|running|none",
				"/geoflow-knowledge-queue-prod|running|none",
				"/geoflow-scheduler-prod|running|none",
				"/geoflow-reverb-prod|running|none",
			}, "\n"),
		},
	}.Run(context.Background(), "primary")
	assertCheck(t, inspectionFailureReport, "retired-update-worker-inspection", doctor.StatusFail)
	assertNoCheck(t, inspectionFailureReport, "retired-update-worker")

	releaseEnvironmentPath := filepath.Join(instanceDir, "release.env")
	releaseEnvironment, err := os.ReadFile(releaseEnvironmentPath)
	if err != nil {
		t.Fatalf("read release environment: %v", err)
	}
	mustWriteFile(t, releaseEnvironmentPath, string(releaseEnvironment)+"GEOFLOW_APP_IMAGE=duplicate\n", 0o640)
	tamperedReport := doctor.Service{
		StateDir: stateDir,
		Probe:    probe{operatingSystem: "linux", commands: map[string]error{}},
	}.Run(context.Background(), "primary")
	assertCheck(t, tamperedReport, "release-pins", doctor.StatusFail)
	assertCheck(t, tamperedReport, "managed-deployment", doctor.StatusFail)
}

func TestDoctorRejectsAnInvalidInstanceIdentifierBeforeJoiningStatePaths(t *testing.T) {
	t.Parallel()

	report := doctor.Service{
		StateDir: t.TempDir(),
		Probe:    probe{operatingSystem: "linux", commands: map[string]error{}},
	}.Run(context.Background(), "../../etc")

	if report.Status != doctor.StatusFail {
		t.Fatalf("doctor status = %q, want fail", report.Status)
	}
	assertCheck(t, report, "instance-id", doctor.StatusFail)
}

func TestDoctorReportsEveryActionableFailure(t *testing.T) {
	t.Parallel()

	report := doctor.Service{
		StateDir: t.TempDir(),
		Probe: probe{
			operatingSystem: "darwin",
			commands:        map[string]error{"docker-compose": os.ErrNotExist},
		},
	}.Run(context.Background(), "missing")

	if report.Status != doctor.StatusFail {
		t.Fatalf("doctor status = %q, want fail", report.Status)
	}
	assertCheck(t, report, "platform", doctor.StatusFail)
	assertCheck(t, report, "docker-compose", doctor.StatusFail)
	assertCheck(t, report, "instance-config", doctor.StatusFail)
}

func assertCheck(t *testing.T, report doctor.Report, id string, status doctor.Status) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			if check.Status != status {
				t.Fatalf("check %q status = %q, want %q", id, check.Status, status)
			}
			return
		}
	}
	t.Fatalf("check %q not found", id)
}

func assertNoCheck(t *testing.T, report doctor.Report, id string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			t.Fatalf("unexpected check %q found", id)
		}
	}
}

func mustWriteFile(t *testing.T, path string, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
