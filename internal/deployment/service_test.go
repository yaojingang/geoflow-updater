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

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"gopkg.in/yaml.v3"
)

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
	wantRestoreStart := []string{"compose", "--env-file", "/opt/geoflow/.env.prod", "--env-file", "/state/release.env", "-f", "/state/docker-compose.yml", "up", "-d", "--wait", "postgres", "redis"}
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
