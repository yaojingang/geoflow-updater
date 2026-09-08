package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

func TestRejectedUpdatePreservesRetainedRelease(t *testing.T) {
	for _, failure := range []string{"pull", "inspect", "plan"} {
		t.Run(failure, func(t *testing.T) {
			state, root := canonicalTemp(t), canonicalTemp(t)
			directory := filepath.Join(state, "instances", "primary")
			next := testRelease(t, true)
			current := instance.Config{SchemaVersion: 1, ID: "primary", Root: root, Layout: LayoutBlueGreen, ActiveSlot: "green", ReleaseSequence: 7, Version: "3.0.0", PostgresMajor: "16", RedisMajor: "7", ControlToken: filepath.Join(directory, "control.token"), InfraComposeFile: filepath.Join(directory, "infra", "docker-compose.yml"), InfraEnvironmentFile: filepath.Join(directory, "infra", "release.env")}
			current.ComposeFile = filepath.Join(directory, "slots", "green", "docker-compose.yml")
			current.EnvironmentFile = filepath.Join(filepath.Dir(current.ComposeFile), "release.env")
			retained := current
			retained.ActiveSlot, retained.ReleaseSequence, retained.Version = "blue", 6, "2.9.0"
			retained.ComposeFile = filepath.Join(directory, "slots", "blue", "docker-compose.yml")
			retained.EnvironmentFile = filepath.Join(filepath.Dir(retained.ComposeFile), "release.env")
			environment := []byte("GEOFLOW_RELEASE_SEQUENCE=7\nGEOFLOW_VERSION=3.0.0\nGEOFLOW_APP_IMAGE=ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("9", 64) + "\nGEOFLOW_WEB_IMAGE=" + next.WebImage + "\nGEOFLOW_POSTGRES_IMAGE=" + next.PostgresImages["16"] + "\nGEOFLOW_REDIS_IMAGE=" + next.RedisImages["7"] + "\n")
			var preserved = map[string][]byte{}
			for _, config := range []instance.Config{current, retained} {
				app, infra, err := renderTopology(next.ComposeTemplate, config, config.ActiveSlot, directory)
				if err != nil {
					t.Fatal(err)
				}
				writeTest(t, config.ComposeFile, app)
				writeTest(t, config.EnvironmentFile, environment)
				writeTest(t, config.InfraComposeFile, infra)
				writeTest(t, config.InfraEnvironmentFile, environment)
			}
			for path, data := range map[string][]byte{retained.ComposeFile: mustReadRetained(t, retained.ComposeFile), retained.EnvironmentFile: environment, filepath.Join(filepath.Dir(retained.ComposeFile), "version.json"): []byte(`{"version":"2.9.0"}`), filepath.Join(filepath.Dir(retained.ComposeFile), "upgrade-plan.json"): next.UpgradePlan} {
				writeTest(t, path, data)
				preserved[path] = data
			}
			configData, _ := yaml.Marshal(current)
			writeTest(t, filepath.Join(directory, "instance.yml"), configData)
			writeTest(t, current.ControlToken, []byte(strings.Repeat("t", 43)))
			writeTest(t, filepath.Join(root, ".env.prod"), []byte("APP_URL=https://site.test\n"))
			writeTest(t, filepath.Join(root, "version.json"), []byte(`{"version":"3.0.0"}`))
			if err := os.MkdirAll(filepath.Join(root, "storage"), 0755); err != nil {
				t.Fatal(err)
			}
			previous := next
			previous.Sequence, previous.Version, previous.VersionDocument = 7, "3.0.0", []byte(`{"version":"3.0.0"}`)
			faultReached := false
			service := Service{StateDir: state, Doctor: fixedDiagnostician{report: doctor.Report{Status: doctor.StatusPass}}, Chown: func(string, int, int) error { return nil }, Runner: functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
				command := strings.Join(args, " ")
				if strings.HasSuffix(command, " pull") && failure == "pull" {
					faultReached = true
					return errors.New("injected pull failure")
				}
				if strings.Contains(command, "geoflow:upgrade") {
					faultReached = true
					if failure == "inspect" {
						return errors.New("injected inspect failure")
					}
					return json.NewEncoder(out).Encode(applicationReport{SchemaVersion: 1, Status: "pass", Version: next.Version, PlanSHA256: managed.PlanSHA256(next.UpgradePlan), EligibleOnline: true})
				}
				return nil
			})}
			previousTx := releaseTransaction{SchemaVersion: 1, OperationID: "previous-update", Status: update.StatusSucceeded, Stage: "succeeded", Strategy: managed.StrategyOnline, Source: retained, Candidate: current, Target: previous, SourceVersion: []byte(`{"version":"2.9.0"}`)}
			if err := service.saveTransaction(&previousTx); err != nil {
				t.Fatal(err)
			}
			preserved[service.transactionPath("primary")] = mustReadRetained(t, service.transactionPath("primary"))
			result := service.ExecuteRelease(context.Background(), "primary", next, update.Options{OperationID: "next-update", ExpectedPlanSHA256: strings.Repeat("0", 64)}, nil)
			if result.Status != update.StatusFailed || !faultReached {
				t.Fatalf("expected preflight rejection, got %+v, fault reached=%v", result, faultReached)
			}
			if failure == "plan" && !strings.Contains(result.Error, "plan changed") {
				t.Fatalf("wrong rejection: %s", result.Error)
			}
			for path, expected := range preserved {
				if data := mustReadRetained(t, path); !bytes.Equal(data, expected) {
					t.Errorf("rejected update overwrote retained release %s", path)
				}
			}
		})
	}
}

func mustReadRetained(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
