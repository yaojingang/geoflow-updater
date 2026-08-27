package deployment

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"gopkg.in/yaml.v3"
)

type fixedDiagnostician struct {
	report doctor.Report
}

func (diagnostician fixedDiagnostician) Run(context.Context, string) doctor.Report {
	return diagnostician.report
}

func TestPreflightOnlyAllowsTheRetiredPhaseBWorkerFailure(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	config := instance.Config{
		SchemaVersion:   1,
		ID:              "primary",
		Root:            root,
		ComposeFile:     filepath.Join(instanceDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(instanceDir, "release.env"),
		ControlToken:    filepath.Join(instanceDir, "control.token"),
		ReleaseSequence: 17,
		Version:         "2.4.0",
		PostgresMajor:   "18",
		RedisMajor:      "8",
	}
	for path, contents := range map[string]string{
		filepath.Join(root, ".env.prod"):    "APP_ENV=production\n",
		filepath.Join(root, "version.json"): "{\"version\":\"2.4.0\"}\n",
		config.ComposeFile:                  "services: {}\n",
		config.EnvironmentFile:              "GEOFLOW_VERSION=2.4.0\n",
		config.ControlToken:                 strings.Repeat("a", 43) + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "instance.yml"), encoded, 0o640); err != nil {
		t.Fatalf("write instance config: %v", err)
	}
	release := managed.Release{
		Sequence:        18,
		Version:         "2.5.0",
		PostgresImages:  map[string]string{"16": "pgvector/pgvector@sha256:" + strings.Repeat("1", 64), "18": "pgvector/pgvector@sha256:" + strings.Repeat("2", 64)},
		RedisImages:     map[string]string{"7": "redis@sha256:" + strings.Repeat("3", 64), "8": "redis@sha256:" + strings.Repeat("4", 64)},
		VersionDocument: []byte(`{"version":"2.5.0"}`),
	}

	transitionReport := doctor.Report{Status: doctor.StatusFail, Checks: []doctor.Check{
		{ID: "platform", Status: doctor.StatusPass},
		{ID: "retired-update-worker", Status: doctor.StatusFail, Message: "Retired Phase B update worker is still present"},
		{ID: "managed-deployment", Status: doctor.StatusPass},
	}}
	service := Service{StateDir: stateDir, Doctor: fixedDiagnostician{report: transitionReport}}
	if err := service.Preflight(context.Background(), "primary", release); err != nil {
		t.Fatalf("Preflight() rejected the bounded Phase B handover: %v", err)
	}

	for _, checkID := range []string{"managed-deployment", "mutation-authorization"} {
		report := transitionReport
		report.Checks = append([]doctor.Check(nil), transitionReport.Checks...)
		report.Checks[1].ID = checkID
		service.Doctor = fixedDiagnostician{report: report}
		if err := service.Preflight(context.Background(), "primary", release); err == nil {
			t.Fatalf("Preflight() accepted unrelated failed check %q", checkID)
		}
	}
}

func TestReplaceEnvironmentValuesUpdatesEveryManagedKeyExactlyOnce(t *testing.T) {
	t.Parallel()

	input := []byte("KEEP=unchanged\nGEOFLOW_VERSION=2.4.0\nGEOFLOW_APP_IMAGE=old\n")
	updated, err := replaceEnvironmentValues(input, map[string]string{
		"GEOFLOW_VERSION":   "2.5.0",
		"GEOFLOW_APP_IMAGE": "ghcr.io/example/app@sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("replaceEnvironmentValues() error = %v", err)
	}
	want := "KEEP=unchanged\nGEOFLOW_VERSION=2.5.0\nGEOFLOW_APP_IMAGE=ghcr.io/example/app@sha256:" + strings.Repeat("a", 64) + "\n"
	if string(updated) != want {
		t.Fatalf("updated environment = %q, want %q", updated, want)
	}
}

func TestReplaceEnvironmentValuesRejectsMissingOrDuplicateManagedKeys(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"missing":   "KEEP=value\n",
		"duplicate": "GEOFLOW_VERSION=2.4.0\nGEOFLOW_VERSION=2.4.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := replaceEnvironmentValues([]byte(input), map[string]string{"GEOFLOW_VERSION": "2.5.0"}); err == nil {
				t.Fatal("replaceEnvironmentValues() accepted unsafe environment")
			}
		})
	}
}

type recordedCommand struct {
	name      string
	arguments []string
	stdin     string
}

type recordingRunner struct {
	commands []recordedCommand
}

type recordingRecoveryStore struct {
	restored bool
}

func (store *recordingRecoveryStore) Validate(instance.Config, string) error {
	return nil
}

func (*recordingRecoveryStore) Create(context.Context, instance.Config, string, recovery.Database) (recovery.Point, error) {
	return recovery.Point{}, nil
}

func (store *recordingRecoveryStore) Restore(context.Context, instance.Config, string, recovery.Database) error {
	store.restored = true
	return nil
}

func (*recordingRecoveryStore) List(string) ([]recovery.Point, error) {
	return nil, nil
}

func (runner *recordingRunner) Run(_ context.Context, stdin io.Reader, _ io.Writer, name string, arguments ...string) error {
	contents := ""
	if stdin != nil {
		read, _ := io.ReadAll(stdin)
		contents = string(read)
	}
	runner.commands = append(runner.commands, recordedCommand{name: name, arguments: append([]string(nil), arguments...), stdin: contents})
	return nil
}

func TestPostgresBackupAndRestoreUseFixedCustomFormatCommands(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	config := instance.Config{Root: "/opt/geoflow", EnvironmentFile: "/state/release.env", ComposeFile: "/state/docker-compose.yml"}
	database := postgresDatabase{config: config, runner: runner}
	var dump bytes.Buffer
	if err := database.Dump(context.Background(), &dump); err != nil {
		t.Fatalf("Dump() error = %v", err)
	}
	if err := database.Restore(context.Background(), strings.NewReader("database dump")); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("commands = %#v", runner.commands)
	}
	common := []string{"compose", "--env-file", "/opt/geoflow/.env.prod", "--env-file", "/state/release.env", "-f", "/state/docker-compose.yml", "exec", "-T", "postgres", "sh", "-eu", "-c"}
	wantRestoreStart := []string{"compose", "--env-file", "/opt/geoflow/.env.prod", "--env-file", "/state/release.env", "-f", "/state/docker-compose.yml", "up", "-d", "--wait", "postgres"}
	wantDump := append(append([]string(nil), common...), `exec pg_dump --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --format=custom --create`)
	wantRestore := append(append([]string(nil), common...), `exec pg_restore --exit-on-error --clean --if-exists --create --username="$POSTGRES_USER" --dbname=postgres`)
	if runner.commands[0].name != "docker" || !reflect.DeepEqual(runner.commands[0].arguments, wantDump) {
		t.Fatalf("dump command = %#v", runner.commands[0])
	}
	if runner.commands[1].name != "docker" || !reflect.DeepEqual(runner.commands[1].arguments, wantRestoreStart) {
		t.Fatalf("restore command = %#v", runner.commands[1])
	}
	if runner.commands[2].name != "docker" || !reflect.DeepEqual(runner.commands[2].arguments, wantRestore) || runner.commands[2].stdin != "database dump" {
		t.Fatalf("restore command = %#v", runner.commands[2])
	}
}

func TestActivatePreservesTheCompleteSignedVersionDocument(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	root := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	config := instance.Config{
		SchemaVersion:   1,
		ID:              "primary",
		Root:            root,
		ComposeFile:     filepath.Join(instanceDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(instanceDir, "release.env"),
		ControlToken:    filepath.Join(instanceDir, "control.token"),
		ReleaseSequence: 17,
		Version:         "2.4.0",
	}
	for path, contents := range map[string]string{
		filepath.Join(root, ".env.prod"):    "APP_ENV=production\n",
		filepath.Join(root, "version.json"): "{\"version\":\"2.4.0\"}\n",
		config.ComposeFile:                  "services: {}\n",
		config.EnvironmentFile:              "GEOFLOW_VERSION=2.4.0\n",
		config.ControlToken:                 strings.Repeat("a", 43) + "\n",
		filepath.Join(instanceDir, "transaction", "docker-compose.candidate.yml"): "services: {}\n",
		filepath.Join(instanceDir, "transaction", "release.candidate.env"):        "GEOFLOW_VERSION=2.5.0\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "instance.yml"), encoded, 0o640); err != nil {
		t.Fatalf("write instance config: %v", err)
	}
	versionDocument := []byte("{\n  \"version\": \"2.5.0\",\n  \"tag\": \"v2.5.0\",\n  \"payload\": {\"title_en\": \"Release metadata\"}\n}\n")
	release := managed.Release{Sequence: 18, Version: "2.5.0", VersionDocument: versionDocument}
	service := Service{StateDir: stateDir}
	if err := service.Activate(context.Background(), "primary", release); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "version.json"))
	if err != nil {
		t.Fatalf("read activated version document: %v", err)
	}
	if !bytes.Equal(actual, versionDocument) {
		t.Fatalf("version document = %q, want %q", actual, versionDocument)
	}
}

func TestRollbackLoadsInstanceWhenStorageSwapWasInterrupted(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	root := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	recoveryPointID := "20260827T123456Z-1234abcd"
	config := instance.Config{
		SchemaVersion:   1,
		ID:              "primary",
		Root:            root,
		ComposeFile:     filepath.Join(instanceDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(instanceDir, "release.env"),
		ControlToken:    filepath.Join(instanceDir, "control.token"),
		ReleaseSequence: 17,
		Version:         "2.4.0",
	}
	for path, contents := range map[string]string{
		filepath.Join(root, ".env.prod"):    "APP_ENV=production\n",
		filepath.Join(root, "version.json"): "{\"version\":\"2.4.0\"}\n",
		config.ComposeFile:                  "services: {}\n",
		config.EnvironmentFile:              "GEOFLOW_VERSION=2.4.0\n",
		config.ControlToken:                 strings.Repeat("a", 43) + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".geoflow-updater-storage-old-"+recoveryPointID), 0o750); err != nil {
		t.Fatalf("mkdir interrupted storage path: %v", err)
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "instance.yml"), encoded, 0o640); err != nil {
		t.Fatalf("write instance config: %v", err)
	}

	recoveries := &recordingRecoveryStore{}
	runner := &recordingRunner{}
	service := Service{StateDir: stateDir, Recoveries: recoveries, Runner: runner}
	if err := service.QuiesceForRecovery(context.Background(), "primary", recoveryPointID); err != nil {
		t.Fatalf("QuiesceForRecovery() error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("QuiesceForRecovery() commands = %#v", runner.commands)
	}
	if !reflect.DeepEqual(runner.commands[1].arguments, []string{"stop", "--time", "900", "geoflow-system-update-queue-prod"}) {
		t.Fatalf("legacy queue stop command = %#v", runner.commands[1])
	}
	wantServices := []string{"queue", "knowledge-queue", "scheduler", "reverb", "web", "app", "redis"}
	if !reflect.DeepEqual(runner.commands[2].arguments[len(runner.commands[2].arguments)-len(wantServices):], wantServices) {
		t.Fatalf("managed service stop command = %#v", runner.commands[2])
	}
	if err := service.Rollback(context.Background(), "primary", recoveryPointID); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if !recoveries.restored {
		t.Fatal("Rollback() did not reach the recovery store")
	}
}

func TestResumeDoesNotRestartTheRetiredPhaseBUpdateWorker(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	instanceDir := filepath.Join(stateDir, "instances", "primary")
	root := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatalf("mkdir site: %v", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	config := instance.Config{
		SchemaVersion:   1,
		ID:              "primary",
		Root:            root,
		ComposeFile:     filepath.Join(instanceDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(instanceDir, "release.env"),
		ControlToken:    filepath.Join(instanceDir, "control.token"),
		ReleaseSequence: 17,
		Version:         "2.4.0",
	}
	for path, contents := range map[string]string{
		filepath.Join(root, ".env.prod"):    "APP_ENV=production\n",
		filepath.Join(root, "version.json"): "{\"version\":\"2.4.0\"}\n",
		config.ComposeFile: "services:\n" +
			"  postgres: {}\n" +
			"  redis: {}\n" +
			"  init: {}\n" +
			"  app: {}\n" +
			"  web: {}\n" +
			"  queue: {}\n" +
			"  knowledge-queue: {}\n" +
			"  system-update-queue: {}\n" +
			"  scheduler: {}\n" +
			"  reverb: {}\n" +
			"  future-worker: {}\n",
		config.EnvironmentFile: "GEOFLOW_VERSION=2.4.0\n",
		config.ControlToken:    strings.Repeat("a", 43) + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "instance.yml"), encoded, 0o640); err != nil {
		t.Fatalf("write instance config: %v", err)
	}

	runner := &recordingRunner{}
	service := Service{StateDir: stateDir, Runner: runner}
	if err := service.Resume(context.Background(), "primary"); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("Resume() commands = %#v", runner.commands)
	}
	if !reflect.DeepEqual(runner.commands[1].arguments, []string{"rm", "-f", "geoflow-system-update-queue-prod"}) {
		t.Fatalf("retired worker removal command = %#v", runner.commands[1])
	}
	wantServices := []string{"app", "future-worker", "init", "knowledge-queue", "postgres", "queue", "redis", "reverb", "scheduler", "web"}
	start := runner.commands[2].arguments
	if len(start) < len(wantServices) || !reflect.DeepEqual(start[len(start)-len(wantServices):], wantServices) {
		t.Fatalf("managed service start command = %#v", runner.commands[2])
	}
	if strings.Contains(strings.Join(start, " "), "system-update-queue") {
		t.Fatalf("retired worker was included in managed service start: %#v", runner.commands[2])
	}
}
