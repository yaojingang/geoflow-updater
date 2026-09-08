package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

func TestOnlineReleaseAndFailuresPreserveDatabase(t *testing.T) {
	for _, failure := range []string{"", "apply", "verify", "persist-switch"} {
		t.Run(failure, func(t *testing.T) {
			state := canonicalTemp(t)
			root := canonicalTemp(t)
			dir := filepath.Join(state, "instances", "primary")
			release := testRelease(t, true)
			config := instance.Config{SchemaVersion: 1, ID: "primary", Root: root, Layout: LayoutBlueGreen, ActiveSlot: "blue", ReleaseSequence: 7, Version: "3.0.0", ComposeFile: filepath.Join(dir, "slots", "blue", "docker-compose.yml"), EnvironmentFile: filepath.Join(dir, "slots", "blue", "release.env"), ControlToken: filepath.Join(dir, "control.token"), InfraComposeFile: filepath.Join(dir, "infra", "docker-compose.yml"), InfraEnvironmentFile: filepath.Join(dir, "infra", "release.env"), PostgresMajor: "16", RedisMajor: "7"}
			env := []byte("GEOFLOW_RELEASE_SEQUENCE=7\nGEOFLOW_VERSION=3.0.0\nGEOFLOW_APP_IMAGE=" + release.AppImage + "\nGEOFLOW_WEB_IMAGE=" + release.WebImage + "\nGEOFLOW_POSTGRES_IMAGE=" + release.PostgresImages["16"] + "\nGEOFLOW_REDIS_IMAGE=" + release.RedisImages["7"] + "\n")
			app, infra, e := renderTopology(release.ComposeTemplate, config, "blue", dir)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := yaml.Marshal(config)
			for path, data := range map[string][]byte{filepath.Join(dir, "instance.yml"): raw, config.ComposeFile: app, config.EnvironmentFile: env, config.InfraComposeFile: infra, config.InfraEnvironmentFile: env, config.ControlToken: []byte(strings.Repeat("t", 43)), filepath.Join(root, ".env.prod"): []byte("APP_URL=https://site.test\n"), filepath.Join(root, "version.json"): []byte(`{"version":"3.0.0"}`)} {
				writeTest(t, path, data)
			}
			if e := os.MkdirAll(filepath.Join(root, "storage"), 0755); e != nil {
				t.Fatal(e)
			}
			active := "blue"
			sequence := uint64(7)
			worker := 1
			var writes []string
			runner := functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
				cmd := strings.Join(args, " ")
				writes = append(writes, cmd)
				if strings.Contains(cmd, "geoflow:upgrade") {
					phase := ""
					for _, arg := range args {
						if strings.HasPrefix(arg, "--phase=") {
							phase = strings.TrimPrefix(arg, "--phase=")
						}
					}
					if failure == phase {
						return errors.New("injected " + phase + " failure")
					}
					data, _ := json.Marshal(applicationReport{SchemaVersion: 1, Status: "pass", Version: release.Version, PlanSHA256: managed.PlanSHA256(release.UpgradePlan), EligibleOnline: true})
					_, e := out.Write(data)
					return e
				}
				switch {
				case strings.HasSuffix(cmd, "ps --status running --quiet edge"):
					_, e := io.WriteString(out, strings.Repeat("b", 12))
					return e
				case strings.HasSuffix(cmd, "ps --all --quiet web"):
					_, e := io.WriteString(out, strings.Repeat("a", 12))
					return e
				case strings.Contains(cmd, "Running}}|"):
					_, e := io.WriteString(out, "false|0|false")
					return e
				case strings.Contains(cmd, "Running}}"):
					_, e := io.WriteString(out, "false")
					return e
				case strings.Contains(cmd, "pg_dump"):
					_, e := io.WriteString(out, "PGDMP-test-snapshot")
					return e
				case len(args) > 0 && args[0] == "cp":
					tw := tar.NewWriter(out)
					data := []byte("asset")
					if e := tw.WriteHeader(&tar.Header{Name: "./app-deadbeef.js", Size: int64(len(data)), Mode: 0644}); e != nil {
						return e
					}
					if _, e := tw.Write(data); e != nil {
						return e
					}
					return tw.Close()
				case strings.Contains(cmd, "nginx -s reload"):
					data, e := os.ReadFile(filepath.Join(dir, "infra", "traffic", "nginx.conf"))
					if e != nil {
						return e
					}
					active = "blue"
					sequence = 7
					if bytes.Contains(data, []byte(`"slot":"green"`)) {
						active = "green"
						sequence = 8
					}
					worker++
					return nil
				case strings.Contains(cmd, "/proc/[0-9]*/cmdline"):
					_, e := io.WriteString(out, fmt.Sprintf("%d:123", worker))
					return e
				case strings.Contains(cmd, "8081/release"):
					return json.NewEncoder(out).Encode(map[string]any{"slot": active, "sequence": sequence})
				case strings.Contains(cmd, "ps --all --format json"):
					var compose string
					for i, arg := range args {
						if arg == "-f" {
							compose = args[i+1]
						}
					}
					names, e := applicationServices(compose)
					if e != nil {
						return e
					}
					for _, n := range names {
						if e := json.NewEncoder(out).Encode(map[string]string{"Service": n, "State": "running", "Health": "healthy"}); e != nil {
							return e
						}
					}
					return nil
				}
				return nil
			})
			store := &recordingRecoveryStore{}
			s := Service{StateDir: state, Runner: runner, Recoveries: store, Doctor: fixedDiagnostician{report: doctor.Report{Status: doctor.StatusPass}}, Chown: func(string, int, int) error { return nil }, ObservationDuration: time.Millisecond}
			observer := func(stage update.Stage) error {
				if failure == "persist-switch" && stage.Name == "switch" && stage.Status == "succeeded" {
					return errors.New("injected journal failure")
				}
				return nil
			}
			result := s.ExecuteRelease(context.Background(), "primary", release, update.Options{OperationID: "online-test"}, observer)
			want := update.StatusSucceeded
			if failure != "" {
				want = update.StatusRolledBack
			}
			if result.Status != want {
				t.Fatalf("status %s error %s", result.Status, result.Error)
			}
			if failure != "" && !strings.Contains(result.Error, "injected") {
				t.Fatal("test failed before intended fault", result.Error)
			}
			if store.restored {
				t.Fatal("online release restored the database")
			}
			for _, cmd := range writes {
				if strings.Contains(cmd, "pg_restore --exit-on-error") || strings.Contains(cmd, "artisan down") {
					t.Fatal("online operation used maintenance/data restoration", cmd)
				}
			}
			current, e := s.loadConfig("primary")
			if e != nil {
				t.Fatal(e)
			}
			if failure == "" && current.ActiveSlot != "green" || failure != "" && current.ActiveSlot != "blue" {
				t.Fatalf("wrong active slot %+v", current)
			}
			if _, e := os.Stat(filepath.Join(dir, "online-backups", "online-test", "metadata.json")); e != nil {
				t.Fatal("snapshot metadata missing", e)
			}
		})
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func writeTest(t *testing.T, path string, data []byte) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func testRelease(t *testing.T, online bool) managed.Release {
	t.Helper()
	compose, e := os.ReadFile("../../assets/docker-compose.managed.yml")
	if e != nil {
		t.Fatal(e)
	}
	strategy := managed.StrategyMaintenance
	if online {
		strategy = managed.StrategyOnline
	}
	plan := managed.UpgradePlan{SchemaVersion: 1, Strategy: strategy, AllowedSources: []uint64{7}, Migrations: []managed.UpgradeMigration{}, Compatibility: managed.Compatibility{Schema: online, Queue: online, Cache: online, Storage: online}, Steps: []managed.UpgradeStep{{ID: "migrate", Kind: "migrate", Phase: "apply", TimeoutSeconds: 30, Online: online}}}
	data, e := json.Marshal(plan)
	if e != nil {
		t.Fatal(e)
	}
	return managed.Release{Sequence: 8, MinimumUpdaterProtocol: 4, Version: "3.1.0", SourceCommit: strings.Repeat("a", 40), AppImage: "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("a", 64), WebImage: "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("b", 64), PostgresImages: map[string]string{"16": "pgvector/pgvector@sha256:" + strings.Repeat("c", 64), "18": "pgvector/pgvector@sha256:" + strings.Repeat("d", 64)}, RedisImages: map[string]string{"7": "redis@sha256:" + strings.Repeat("e", 64), "8": "redis@sha256:" + strings.Repeat("f", 64)}, ComposeTemplate: compose, VersionDocument: []byte(`{"version":"3.1.0"}`), UpgradePlan: data}
}

func TestTopologySeparatesSlotsAndKeepsEveryDomainWorker(t *testing.T) {
	release := testRelease(t, true)
	c := instance.Config{ID: "primary", ReleaseSequence: 8}
	app, infra, e := renderTopology(release.ComposeTemplate, c, "green", "/state/instances/primary")
	if e != nil {
		t.Fatal(e)
	}
	var a, b map[string]any
	if e := yaml.Unmarshal(app, &a); e != nil {
		t.Fatal(e)
	}
	if e := yaml.Unmarshal(infra, &b); e != nil {
		t.Fatal(e)
	}
	apps := a["services"].(map[string]any)
	infrastructure := b["services"].(map[string]any)
	for _, name := range []string{"ai-quality-queue", "ai-quality-backfill-queue", "ai-optimization-queue", "queue", "knowledge-queue", "scheduler", "reverb", "app", "web", "init"} {
		if apps[name] == nil {
			t.Fatalf("lost %s", name)
		}
	}
	if apps["postgres"] != nil || apps["redis"] != nil || len(infrastructure) != 3 {
		t.Fatal("infrastructure not separated")
	}
	for name, raw := range apps {
		s := raw.(map[string]any)
		for _, key := range []string{"container_name", "ports", "depends_on", "build"} {
			if _, ok := s[key]; ok {
				t.Fatalf("%s retained %s", name, key)
			}
		}
	}
	if !bytes.Contains(app, []byte("REVERB_SCALING_ENABLED: \"true\"")) || !bytes.Contains(app, []byte("slots/green/views")) {
		t.Fatal("missing cross-slot contracts")
	}
	if bytes.Contains(app, []byte("geoflow-prod-net")) {
		t.Fatal("slot inherited global app DNS network")
	}
	if !bytes.Contains(infra, []byte("/infra/traffic:/etc/geoflow:ro")) {
		t.Fatal("ingress must mount a directory for atomic replacement")
	}
	if _, _, e := renderTopology(release.ComposeTemplate, c, "../escape", "/state"); e == nil {
		t.Fatal("accepted slot escape")
	}
}

func TestSafeDirectoryRejectsSymlinkBeforeCreatingDescendants(t *testing.T) {
	root := canonicalTemp(t)
	outside := canonicalTemp(t)
	if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if e := safeDirectory(filepath.Join(root, "link", "new", "nested"), 0700); e == nil {
		t.Fatal("accepted symbolic path")
	}
	if _, e := os.Stat(filepath.Join(outside, "new")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("created directory outside owned state")
	}
}

func TestResumeIngressStartsAnAbsentEdge(t *testing.T) {
	state := canonicalTemp(t)
	root := canonicalTemp(t)
	writeTest(t, filepath.Join(root, ".env.prod"), []byte("APP_URL=https://example.test\n"))
	var calls []string
	config := instance.Config{ID: "primary", Root: root, Layout: LayoutBlueGreen, ActiveSlot: "blue", ReleaseSequence: 7, InfraComposeFile: filepath.Join(state, "infra.yml"), InfraEnvironmentFile: filepath.Join(state, "infra.env")}
	service := Service{StateDir: state, Runner: functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		if strings.Contains(cmd, "8081/release") {
			return json.NewEncoder(out).Encode(map[string]any{"slot": "blue", "sequence": 7})
		}
		return nil
	})}
	if _, err := service.switchIngress(context.Background(), config, 7, true); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(calls, "\n")
	if !strings.Contains(commands, "up -d --no-deps --wait --wait-timeout 60 edge") || strings.Contains(commands, "nginx -s reload") {
		t.Fatal("absent ingress was not started", commands)
	}
}

type functionRunner func(context.Context, io.Reader, io.Writer, string, ...string) error

func (f functionRunner) Run(c context.Context, i io.Reader, o io.Writer, n string, a ...string) error {
	return f(c, i, o, n, a...)
}

func TestDrainDeadlineRetainsRunningWorkerWithoutKillEscalation(t *testing.T) {
	var calls []string
	s := Service{Runner: functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch {
		case strings.Contains(command, "ps --all --quiet"):
			io.WriteString(out, strings.Repeat("a", 12))
		case strings.Contains(command, "Running}}|"):
			io.WriteString(out, "true|0|false")
		case strings.Contains(command, "Running}}"):
			io.WriteString(out, "true")
		}
		return nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if e := s.drainServices(ctx, instance.Config{}, "queue"); e == nil || !strings.Contains(e.Error(), "retained") {
		t.Fatalf("expected retained timeout, got %v", e)
	}
	found := false
	for _, call := range calls {
		if strings.Contains(call, "--signal=TERM") {
			found = true
		}
		if strings.Contains(call, "--signal=KILL") || strings.HasPrefix(call, "stop ") || strings.HasPrefix(call, "rm ") {
			t.Fatal("destructive drain", call)
		}
	}
	if !found {
		t.Fatal("worker was never asked to drain")
	}
}

func TestMaintenanceFailureAfterTrafficNeverRestoresData(t *testing.T) {
	root := canonicalTemp(t)
	state := canonicalTemp(t)
	dir := filepath.Join(state, "instances", "primary")
	writeTest(t, filepath.Join(dir, "instance.yml"), []byte("fixture"))
	store := &recordingRecoveryStore{}
	s := Service{StateDir: state, Recoveries: store, Runner: &recordingRunner{}}
	tx := releaseTransaction{Source: instance.Config{ID: "primary", Root: root}, Target: testRelease(t, false), Strategy: managed.StrategyMaintenance, Stage: "observe", TrafficOpened: true, RecoveryPointID: "checkpoint", OperationID: "test"}
	result := s.failRelease(context.Background(), &tx, errors.New("bad candidate"), nil)
	if result.Status != update.StatusRecoveryRequired || store.restored {
		t.Fatalf("unsafe post-write recovery: %+v", result)
	}
	if !strings.Contains(result.Error, "separate authorized recovery") {
		t.Fatal("missing actionable recovery explanation")
	}
}

func TestPlanHashBindsSourceEnvironmentAndPendingMigrations(t *testing.T) {
	root := canonicalTemp(t)
	env := filepath.Join(root, ".env.prod")
	writeTest(t, env, []byte("APP_URL=https://site.test\n"))
	release := testRelease(t, true)
	infraEnv := filepath.Join(root, "infra.env")
	writeTest(t, infraEnv, []byte("GEOFLOW_POSTGRES_IMAGE="+release.PostgresImages["16"]+"\nGEOFLOW_REDIS_IMAGE="+release.RedisImages["7"]+"\n"))
	config := instance.Config{ID: "primary", Root: root, Layout: LayoutBlueGreen, ActiveSlot: "blue", ReleaseSequence: 7, PostgresMajor: "16", RedisMajor: "7", InfraEnvironmentFile: infraEnv}
	s := Service{}
	report := applicationReport{EligibleOnline: true}
	first, e := s.summarize(config, release, report)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, env, []byte("APP_URL=https://changed.test\n"))
	second, e := s.summarize(config, release, report)
	if e != nil {
		t.Fatal(e)
	}
	if first.PlanSHA256 == second.PlanSHA256 {
		t.Fatal("environment change did not invalidate plan")
	}
	report.PendingMigrations = []managed.UpgradeMigration{{Name: "2026_01_01_000001_create_table", SHA256: strings.Repeat("a", 64), Online: true}}
	third, e := s.summarize(config, release, report)
	if e != nil {
		t.Fatal(e)
	}
	if second.PlanSHA256 == third.PlanSHA256 {
		t.Fatal("migration state did not invalidate plan")
	}
	config.Layout = ""
	fourth, e := s.summarize(config, release, report)
	if e != nil || fourth.Strategy != managed.StrategyMaintenance || !fourth.LayoutChange {
		t.Fatalf("legacy must require maintenance: %+v %v", fourth, e)
	}
}
