package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"gopkg.in/yaml.v3"
)

func TestRecoveryRuntimeMountsOnePublicDirectoryInEveryPHPService(t *testing.T) {
	service, legacy, _, _ := recoveryTopologyFixture(t)
	control := recoverycontrol.Store{StateDir: service.StateDir}
	if _, err := control.Initialize(legacy.ID); err != nil {
		t.Fatal(err)
	}
	signed, err := os.ReadFile(legacy.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	var arguments []string
	service.Runner = functionRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
		arguments = args
		return nil
	})
	if err := service.command(context.Background(), legacy, "config", "--quiet"); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(filepath.Dir(legacy.ComposeFile), "recovery-runtime.yml")
	if !strings.Contains(strings.Join(arguments, " "), "-f "+overlay) {
		t.Fatalf("PHP services launched without recovery overlay: %v", arguments)
	}
	data, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", "init", "queue", "knowledge-queue", "scheduler", "reverb"} {
		svc, ok := document.Services[name]
		if !ok || svc.Environment["GEOFLOW_RECOVERY_CONTRACT"] != "1" || len(svc.Volumes) != 1 || svc.Volumes[0] != control.PublicDir(legacy.ID)+":/run/geoflow-recovery-control:ro" {
			t.Errorf("missing public-only directory mount for %s: %+v", name, svc)
		}
	}
	after, _ := os.ReadFile(legacy.ComposeFile)
	if string(after) != string(signed) {
		t.Fatal("runtime overlay modified signed compose bytes")
	}
}

func TestMaintenanceRecoveryInspectsBeforeRemovingDatabaseNetwork(t *testing.T) {
	service, legacy, _, tx := recoveryTopologyFixture(t)
	tx.TrafficOpened = false
	removed, restored := false, false
	service.Recoveries = &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &legacy}, restore: func() error { restored = true; return errors.New("restore boundary reached") }}
	service.Runner = recoveryFixtureRunner(func(_ context.Context, _ io.Reader, _ io.Writer, _ string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "down --remove-orphans") {
			removed = true
		}
		if isRecoveryInspect(args) && removed {
			return errors.New("administrator inspection lost its database network")
		}
		return nil
	})
	err := service.restoreBeforeTraffic(context.Background(), &tx)
	if !restored {
		t.Fatalf("restore did not inspect administrators while the database was reachable: %v", err)
	}
}

func TestRecoveryResumeRequiresPreparationAndKeepsBackgroundStopped(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "credential-verification-fails"}[failed], func(t *testing.T) {
			service, config, _, _ := recoveryTopologyFixture(t)
			writeTest(t, config.EnvironmentFile, []byte("GEOFLOW_APP_IMAGE=ghcr.io/yaojingang/geoflow-app@sha256:"+strings.Repeat("a", 64)+"\nGEOFLOW_WEB_IMAGE="+recoveryWebFixture+"\n"))
			control := service.recoveryControl()
			a, err := control.Begin(config.ID, "20260827T123456Z-1234abcd", "recovery-transaction", strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = control.Restored(config.ID, "recovery-transaction"); err != nil {
				t.Fatal(err)
			}
			var starts []string
			docker := newRecoveryHTTPDocker(t, service, config)
			docker.before = functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
				cmd := strings.Join(args, " ")
				if strings.Contains(cmd, "geoflow:recovery") {
					return json.NewEncoder(out).Encode(map[string]any{"schema_version": 1, "status": "pass", "transaction_id": "recovery-transaction", "epoch": a.State.Epoch, "credentials_invalidated": !failed, "administrators_verified": true, "theme_revisions": 0, "background_status": "held", "quarantine_count": 0, "quarantine_sha256": strings.Repeat("b", 64)})
				}
				if strings.Contains(cmd, "recovery-redis.php") {
					_, err := io.WriteString(out, `{"schema_version":1,"status":"pass","epoch":"`+a.State.Epoch+`","entries":[]}`)
					return err
				}
				if strings.Contains(cmd, " up -d") {
					starts = append(starts, cmd)
				}
				return nil
			})
			service.Runner = docker
			err = service.Resume(context.Background(), config.ID)
			if failed && err == nil {
				t.Fatal("HTTP opened without credential invalidation proof")
			}
			if !failed && err != nil {
				t.Fatal(err)
			}
			for _, cmd := range starts {
				if strings.Contains(cmd, "queue") || strings.Contains(cmd, "scheduler") || (failed && (strings.Contains(cmd, " app") || strings.Contains(cmd, " web"))) {
					t.Errorf("unsafe resumed services: %s", cmd)
				}
			}
			a, err = control.Read(config.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "http_ready"
			if failed {
				want = "validating"
			}
			if a.State.Phase != want {
				t.Fatalf("phase=%s want=%s", a.State.Phase, want)
			}
		})
	}
}
