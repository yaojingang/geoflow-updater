package deployment

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

type topologyRecoveryStore struct {
	recordingRecoveryStore
	point   recovery.Point
	restore func() error
}

func (s *topologyRecoveryStore) List(string) ([]recovery.Point, error) {
	return []recovery.Point{s.point}, nil
}
func (s *topologyRecoveryStore) Restore(context.Context, instance.Config, string, recovery.Database) error {
	return s.restore()
}

func recoveryTopologyFixture(t *testing.T) (*Service, instance.Config, instance.Config, releaseTransaction) {
	t.Helper()
	state, root := canonicalTemp(t), canonicalTemp(t)
	dir := filepath.Join(state, "instances", "primary")
	release := testRelease(t, false)
	legacy := instance.Config{SchemaVersion: 1, ID: "primary", Root: root, ComposeFile: filepath.Join(dir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(dir, "release.env"), ControlToken: filepath.Join(dir, "control.token"), Version: release.Version, ReleaseSequence: 7, PostgresMajor: "16", RedisMajor: "7"}
	candidate := legacy
	candidate.Layout, candidate.ActiveSlot, candidate.ReleaseSequence = LayoutBlueGreen, "blue", 8
	candidate.ComposeFile = filepath.Join(dir, "slots", "blue", "docker-compose.yml")
	candidate.EnvironmentFile = filepath.Join(dir, "slots", "blue", "release.env")
	candidate.InfraComposeFile = filepath.Join(dir, "infra", "docker-compose.yml")
	candidate.InfraEnvironmentFile = filepath.Join(dir, "infra", "release.env")
	app, infra, err := renderTopology(release.ComposeTemplate, candidate, "blue", dir)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := yaml.Marshal(legacy)
	for path, data := range map[string][]byte{
		filepath.Join(dir, "instance.yml"): encoded, legacy.ComposeFile: release.ComposeTemplate,
		legacy.EnvironmentFile: []byte("VERSION=3.0.0\n"), legacy.ControlToken: []byte("token"),
		candidate.ComposeFile: app, candidate.EnvironmentFile: []byte("VERSION=3.0.0\n"),
		candidate.InfraComposeFile: infra, candidate.InfraEnvironmentFile: []byte("VERSION=3.0.0\n"),
		filepath.Join(root, ".env.prod"): []byte("APP_URL=https://example.test\n"), filepath.Join(root, "version.json"): []byte(`{"version":"3.0.0"}`),
	} {
		writeTest(t, path, data)
	}
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0755); err != nil {
		t.Fatal(err)
	}
	tx := releaseTransaction{SchemaVersion: 1, OperationID: "interrupted-switch", Status: update.StatusRecoveryRequired, Stage: "switch", Source: legacy, Candidate: candidate, Target: release, LayoutStarted: true, TrafficOpened: true, RecoveryPointID: "20260909T010000Z-1234abcd"}
	service := &Service{StateDir: state}
	if err := service.saveTransaction(&tx); err != nil {
		t.Fatal(err)
	}
	return service, legacy, candidate, tx
}

func TestRecoveryStopsPendingLayoutBeforeRestoringAnyCheckpoint(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-point", true: "another-point-after-restart"}[retry], func(t *testing.T) {
			service, legacy, candidate, tx := recoveryTopologyFixture(t)
			pointID := tx.RecoveryPointID
			if retry {
				pointID = "20260908T010000Z-5678abcd"
			}
			var calls []string
			service.Runner = functionRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			})
			store := &topologyRecoveryStore{point: recovery.Point{ID: pointID, Deployment: &legacy}}
			service.Recoveries = store
			attempts := 0
			store.restore = func() error {
				joined := strings.Join(calls, "\n")
				for _, expected := range []string{candidate.ComposeFile + " ps --all --quiet scheduler", candidate.ComposeFile + " ps --all --quiet reverb", candidate.ComposeFile + " ps --all --quiet web", candidate.ComposeFile + " ps --all --quiet app", candidate.InfraComposeFile + " ps --all --quiet redis", candidate.ComposeFile + " down --remove-orphans", candidate.InfraComposeFile + " down --remove-orphans"} {
					if !strings.Contains(joined, expected) {
						t.Errorf("restoring data before stopping pending topology: missing %s", expected)
					}
				}
				attempts++
				if retry && attempts == 1 {
					return errors.New("interrupted after managed files were restored")
				}
				return nil
			}
			for i := 0; i < 1+map[bool]int{false: 0, true: 1}[retry]; i++ {
				if err := service.QuiesceForRecovery(context.Background(), legacy.ID, pointID); err != nil {
					t.Fatal(err)
				}
				err := service.Rollback(context.Background(), legacy.ID, pointID)
				if retry && i == 0 {
					if err == nil {
						t.Fatal("missing simulated restore failure")
					}
					service = &Service{StateDir: service.StateDir, Runner: service.Runner, Recoveries: store}
					calls = nil
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRecoveryRejectsUnsafePendingTopologyBeforeDocker(t *testing.T) {
	for _, kind := range []string{"root", "infra-path", "infra-symlink", "other-slot-parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			service, legacy, candidate, tx := recoveryTopologyFixture(t)
			switch kind {
			case "root":
				tx.Candidate.Root = canonicalTemp(t)
			case "infra-path":
				tx.Candidate.InfraComposeFile = filepath.Join(canonicalTemp(t), "foreign.yml")
			case "infra-symlink":
				data, err := os.ReadFile(candidate.InfraComposeFile)
				if err != nil {
					t.Fatal(err)
				}
				foreign := filepath.Join(canonicalTemp(t), "foreign.yml")
				writeTest(t, foreign, data)
				if err := os.Remove(candidate.InfraComposeFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, candidate.InfraComposeFile); err != nil {
					t.Fatal(err)
				}
			case "other-slot-parent-symlink":
				foreign := canonicalTemp(t)
				writeTest(t, filepath.Join(foreign, "docker-compose.yml"), []byte("services: {}\n"))
				writeTest(t, filepath.Join(foreign, "release.env"), []byte("VERSION=3.0.0\n"))
				if err := os.Symlink(foreign, filepath.Join(service.instanceDirectory(legacy.ID), "slots", "green")); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.saveTransaction(&tx); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			service.Runner = runner
			if err := service.QuiesceForRecovery(context.Background(), legacy.ID, tx.RecoveryPointID); err == nil {
				t.Fatal("unsafe transaction accepted")
			}
			if len(runner.commands) != 0 {
				t.Fatal("Docker invoked before topology validation")
			}
		})
	}
}

func TestRecoveryClosesOnlyInfrastructureDifferentFromDestination(t *testing.T) {
	for _, currentBlue := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-to-blue-green", true: "same-blue-green-infra"}[currentBlue], func(t *testing.T) {
			service, legacy, candidate, tx := recoveryTopologyFixture(t)
			tx.Status = update.StatusRolledBack
			if err := service.saveTransaction(&tx); err != nil {
				t.Fatal(err)
			}
			if currentBlue {
				encoded, _ := yaml.Marshal(candidate)
				writeTest(t, filepath.Join(service.instanceDirectory(legacy.ID), "instance.yml"), encoded)
			}
			var calls []string
			service.Runner = functionRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			})
			store := &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &candidate}}
			service.Recoveries = store
			store.restore = func() error {
				joined := strings.Join(calls, "\n")
				if !currentBlue && !strings.Contains(joined, legacy.ComposeFile+" down --remove-orphans") {
					t.Error("legacy database can remain running beside restored blue-green database")
				}
				if strings.Contains(joined, candidate.InfraComposeFile+" down --remove-orphans") {
					t.Error("stopped the destination infrastructure")
				}
				if strings.Contains(joined, candidate.ComposeFile+" down --remove-orphans") {
					t.Error("removed the destination slot")
				}
				return nil
			}
			if err := service.Rollback(context.Background(), legacy.ID, tx.RecoveryPointID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInfrastructureReplacementRemovesBothSlotsBeforeNetworks(t *testing.T) {
	for _, path := range []string{"rollback", "before-traffic", "maintenance-upgrade"} {
		for _, failedSlot := range []string{"", "blue", "green"} {
			t.Run(path+"/failed-slot="+failedSlot, func(t *testing.T) {
				service, legacy, candidate, tx := recoveryTopologyFixture(t)
				other := candidate
				other.ActiveSlot = "green"
				other.ComposeFile = filepath.Join(service.instanceDirectory(legacy.ID), "slots", "green", "docker-compose.yml")
				other.EnvironmentFile = filepath.Join(filepath.Dir(other.ComposeFile), "release.env")
				data, err := os.ReadFile(candidate.ComposeFile)
				if err != nil {
					t.Fatal(err)
				}
				writeTest(t, other.ComposeFile, data)
				writeTest(t, other.EnvironmentFile, []byte("VERSION=3.0.0\n"))
				var removed []string
				boundary := errors.New("teardown boundary")
				service.Runner = functionRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
					if !strings.Contains(strings.Join(args, " "), " down --remove-orphans") {
						return nil
					}
					for i, arg := range args {
						if arg == "-f" {
							file := args[i+1]
							removed = append(removed, file)
							if file == candidate.InfraComposeFile || (failedSlot != "" && file == filepath.Join(service.instanceDirectory(legacy.ID), "slots", failedSlot, "docker-compose.yml")) {
								return boundary
							}
						}
					}
					return nil
				})
				service.Recoveries = &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &legacy}, restore: func() error {
					t.Fatal("restoration started after teardown failure")
					return nil
				}}
				switch path {
				case "rollback":
					err = service.Rollback(context.Background(), legacy.ID, tx.RecoveryPointID)
				case "before-traffic":
					err = service.restoreBeforeTraffic(context.Background(), &tx)
				case "maintenance-upgrade":
					err = service.installInfrastructure(context.Background(), candidate, other, tx.Target)
				}
				if !errors.Is(err, boundary) {
					t.Fatalf("teardown failure not returned: %v", err)
				}
				want := []string{candidate.ComposeFile}
				if failedSlot != "blue" {
					want = append(want, other.ComposeFile)
				}
				if failedSlot == "" {
					want = append(want, candidate.InfraComposeFile)
				}
				if strings.Join(removed, "\n") != strings.Join(want, "\n") {
					t.Fatalf("teardown order: got %v, want %v", removed, want)
				}
			})
		}
	}
}
