package deployment

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"gopkg.in/yaml.v3"
)

func enrolledConversionFixture(t *testing.T) (*Service, managed.Release, instance.Config) {
	t.Helper()
	state, root := canonicalTemp(t), canonicalTemp(t)
	release := testRelease(t, false)
	plan, err := release.Plan()
	if err != nil {
		t.Fatal(err)
	}
	plan.AllowedSources = []uint64{}
	release.UpgradePlan, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, ".env.prod"), []byte("APP_URL=https://site.test\nAPP_KEY=base64:"+strings.Repeat("A", 43)+"=\nDB_CONNECTION=pgsql\nDB_HOST=postgres\nDB_PASSWORD=test-only\nREDIS_HOST=redis\nPGVECTOR_IMAGE=pgvector/pgvector:pg16\nREDIS_IMAGE=redis:7-alpine\nPOSTGRES_DATA_DIR=./docker-data/prod/postgres\nPOSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql/data\n"))
	writeTest(t, filepath.Join(root, "version.json"), release.VersionDocument)
	writeTest(t, filepath.Join(root, "docker-data/prod/postgres/PG_VERSION"), []byte("16\n"))
	writeTest(t, filepath.Join(root, "docker-data/prod/redis/dump.rdb"), []byte("redis-fixture"))
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0750); err != nil {
		t.Fatal(err)
	}
	resolver := releaseResolverFunc(func(context.Context) (managed.Release, error) { return release, nil })
	enroller := enrollment.Service{StateDir: state, Releases: resolver, RootAccess: func(string) error { return nil }, ControlGroupID: func() (int, error) { return 991, nil }, Chown: func(string, int, int) error { return nil }}
	enrolled, err := enroller.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{StateDir: state, Releases: resolver, Doctor: fixedDiagnostician{report: doctor.Report{Status: doctor.StatusPass}}, Chown: func(string, int, int) error { return nil }}
	service.Runner = functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "geoflow:upgrade") {
			return json.NewEncoder(out).Encode(applicationReport{SchemaVersion: 1, Status: "pass", Version: release.Version, PlanSHA256: managed.PlanSHA256(release.UpgradePlan)})
		}
		return nil
	})
	return service, release, enrolled.Instance
}

func TestEnrolledCurrentReleaseCanPreviewMaintenanceLayoutConversion(t *testing.T) {
	service, release, _ := enrolledConversionFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		plan, err := service.Preview(context.Background(), "primary")
		if err != nil {
			t.Fatalf("newly enrolled current release cannot preview its layout conversion: %v", err)
		}
		if !plan.LayoutChange || plan.Strategy != managed.StrategyMaintenance || plan.SourceSequence != release.Sequence || plan.TargetSequence != release.Sequence {
			t.Fatalf("unexpected conversion plan: %+v", plan)
		}
	}
}

func TestEnrolledCurrentReleaseConvertsOnceWithPreviewAndMaintenanceConsent(t *testing.T) {
	service, release, enrolled := enrolledConversionFixture(t)
	preview, err := service.Preview(context.Background(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	service.Runner = conversionRunner(t, release, &calls)
	service.Recoveries = recovery.Store{BackupRoot: canonicalTemp(t)}
	service.ObservationDuration = time.Nanosecond
	options := update.Options{OperationID: "enrolled-conversion", ExpectedPlanSHA256: preview.PlanSHA256}
	result := service.ExecuteRelease(context.Background(), "primary", release, options, nil)
	if result.Status != update.StatusFailed || !strings.Contains(result.Error, "maintenance window") {
		t.Fatalf("conversion bypassed maintenance consent: %+v", result)
	}
	for _, call := range calls {
		if strings.Contains(call, "artisan down") || strings.Contains(call, "up -d") {
			t.Fatalf("conversion mutated services before maintenance consent: %s", call)
		}
	}
	options.AllowMaintenance = true
	result = service.ExecuteRelease(context.Background(), "primary", release, options, nil)
	if result.Status != update.StatusSucceeded {
		t.Fatalf("conversion failed: %+v", result)
	}
	current, err := service.loadConfig("primary")
	if err != nil || current.Layout != LayoutBlueGreen || current.ActiveSlot != "blue" || current.ReleaseSequence != enrolled.ReleaseSequence || current.EnrolledReleaseSHA256 != enrolled.EnrolledReleaseSHA256 {
		t.Fatalf("converted instance lost its release identity: %+v %v", current, err)
	}
	if _, err := service.Preview(context.Background(), "primary"); err == nil {
		t.Fatal("already converted release accepted another same-sequence preview")
	}
	before := len(calls)
	for _, operation := range []string{options.OperationID, "repeat-conversion"} {
		options.OperationID = operation
		if result := service.ExecuteRelease(context.Background(), "primary", release, options, nil); result.Status != update.StatusFailed {
			t.Fatalf("already converted release accepted repeat request: %+v", result)
		}
	}
	if len(calls) != before {
		t.Fatal("repeat conversion reached Docker")
	}
	if err := service.QuiesceForRecovery(context.Background(), "primary", result.RecoveryPointID); err != nil {
		t.Fatal(err)
	}
	if err := service.Rollback(context.Background(), "primary", result.RecoveryPointID); err != nil {
		t.Fatalf("full recovery failed: %v", err)
	}
	restored, err := service.loadConfig("primary")
	if err != nil || restored.Layout != "" || restored.EnrolledReleaseSHA256 != enrolled.EnrolledReleaseSHA256 {
		t.Fatalf("recovery lost the enrolled release proof: %+v %v", restored, err)
	}
	if _, err := service.Preview(context.Background(), "primary"); err != nil {
		t.Fatalf("restored enrollment can no longer retry conversion: %v", err)
	}
	options.OperationID = "conversion-after-recovery"
	if result := service.ExecuteRelease(context.Background(), "primary", release, options, nil); result.Status != update.StatusSucceeded {
		t.Fatalf("conversion failed after full recovery: %+v", result)
	}
}

func conversionRunner(t *testing.T, release managed.Release, calls *[]string) CommandRunner {
	t.Helper()
	return functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
		command := strings.Join(args, " ")
		*calls = append(*calls, command)
		switch {
		case strings.Contains(command, "pg_dump"):
			_, err := io.WriteString(out, "PGDMP-fixture")
			return err
		case strings.Contains(command, "geoflow:upgrade"):
			return json.NewEncoder(out).Encode(applicationReport{SchemaVersion: 1, Status: "pass", Version: release.Version, PlanSHA256: managed.PlanSHA256(release.UpgradePlan)})
		case strings.HasSuffix(command, "ps --all --quiet web"):
			_, err := io.WriteString(out, strings.Repeat("a", 12))
			return err
		case strings.Contains(command, "Running}}|"):
			_, err := io.WriteString(out, "false|0|false")
			return err
		case strings.Contains(command, "Running}}"):
			_, err := io.WriteString(out, "false")
			return err
		case len(args) > 0 && args[0] == "cp":
			archive := tar.NewWriter(out)
			data := []byte("retained asset")
			if err := archive.WriteHeader(&tar.Header{Name: "app-abcd.js", Size: int64(len(data)), Mode: 0644}); err != nil {
				return err
			}
			if _, err := archive.Write(data); err != nil {
				return err
			}
			return archive.Close()
		case strings.Contains(command, "8081/release"):
			return json.NewEncoder(out).Encode(map[string]any{"slot": "blue", "sequence": release.Sequence})
		case strings.HasSuffix(command, "ps --all --format json"):
			var compose string
			for i, arg := range args {
				if arg == "-f" {
					compose = args[i+1]
				}
			}
			names, err := applicationServices(compose)
			if err != nil {
				return err
			}
			for _, name := range names {
				if err := json.NewEncoder(out).Encode(map[string]string{"Service": name, "State": "running", "Health": "healthy"}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func TestEnrolledConversionRejectsChangedIdentityAndUnsupportedPlans(t *testing.T) {
	for _, conflict := range []string{"downgrade", "missing-proof", "wrong-proof", "version", "source", "app-image", "web-image", "postgres-image", "redis-image", "compose", "version-document", "plan", "online", "missing-plan", "source-not-allowed", "installed-compose", "installed-version-document", "installed-pin", "duplicate-pin"} {
		t.Run(conflict, func(t *testing.T) {
			service, release, config := enrolledConversionFixture(t)
			switch conflict {
			case "downgrade":
				release.Sequence--
			case "missing-proof":
				config.EnrolledReleaseSHA256 = ""
			case "wrong-proof":
				config.EnrolledReleaseSHA256 = strings.Repeat("0", 64)
			case "version":
				release.Version = "3.1.1"
				release.VersionDocument = []byte(`{"version":"3.1.1"}`)
			case "source":
				release.SourceCommit = strings.Repeat("b", 40)
			case "app-image":
				release.AppImage = "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64)
			case "web-image":
				release.WebImage = "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("1", 64)
			case "postgres-image":
				release.PostgresImages["16"] = "pgvector/pgvector@sha256:" + strings.Repeat("1", 64)
			case "redis-image":
				release.RedisImages["7"] = "redis@sha256:" + strings.Repeat("1", 64)
			case "compose":
				release.ComposeTemplate = append(release.ComposeTemplate, []byte("\n# changed\n")...)
			case "version-document":
				release.VersionDocument = []byte(`{"version":"3.1.0","changed":true}`)
			case "missing-plan":
				release.UpgradePlan = nil
			case "plan", "online", "source-not-allowed":
				plan, err := release.Plan()
				if err != nil {
					t.Fatal(err)
				}
				plan.Steps[0].TimeoutSeconds++
				if conflict == "online" {
					plan.Strategy = managed.StrategyOnline
					plan.AllowedSources = []uint64{release.Sequence}
					plan.Compatibility = managed.Compatibility{Schema: true, Queue: true, Cache: true, Storage: true}
					plan.Steps[0].Online = true
				} else if conflict == "source-not-allowed" {
					plan.AllowedSources = []uint64{release.Sequence - 1}
				}
				release.UpgradePlan, err = json.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				if conflict == "online" || conflict == "source-not-allowed" {
					identity, err := json.Marshal(release)
					if err != nil {
						t.Fatal(err)
					}
					config.EnrolledReleaseSHA256 = fmt.Sprintf("%x", sha256.Sum256(identity))
				}
			case "installed-compose":
				writeTest(t, config.ComposeFile, append(release.ComposeTemplate, []byte("\n# local edit\n")...))
			case "installed-version-document":
				writeTest(t, filepath.Join(config.Root, "version.json"), []byte(`{"version":"3.1.0","changed":true}`))
			case "installed-pin", "duplicate-pin":
				data, err := os.ReadFile(config.EnvironmentFile)
				if err != nil {
					t.Fatal(err)
				}
				if conflict == "installed-pin" {
					data = []byte(strings.ReplaceAll(string(data), release.AppImage, "ghcr.io/yaojingang/geoflow-app@sha256:"+strings.Repeat("1", 64)))
				} else {
					data = append(data, []byte("GEOFLOW_APP_IMAGE="+release.AppImage+"\n")...)
				}
				writeTest(t, config.EnvironmentFile, data)
			}
			raw, err := yaml.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(config.StateDirectory(), "instance.yml"), raw)
			service.Releases = releaseResolverFunc(func(context.Context) (managed.Release, error) { return release, nil })
			service.Runner = functionRunner(func(context.Context, io.Reader, io.Writer, string, ...string) error {
				t.Fatal("rejected identity reached Docker")
				return nil
			})
			if _, err := service.Preview(context.Background(), "primary"); err == nil {
				t.Fatal("conflicting release accepted for preview")
			}
			result := service.ExecuteRelease(context.Background(), "primary", release, update.Options{OperationID: "rejected", AllowMaintenance: true}, nil)
			if result.Status != update.StatusFailed {
				t.Fatalf("conflicting release accepted for execution: %+v", result)
			}
		})
	}
}
