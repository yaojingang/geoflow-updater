package deployment

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	trust "github.com/yaojingang/geoflow-updater/tuf"
)

func TestLegacyAdapterPinsTheExactSignedCoreBuild(t *testing.T) {
	root, err := metadata.Root().FromBytes(trust.TrustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := metadata.Targets().FromFile("../../tuf/repository/metadata/5.targets.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = root.VerifyDelegate("targets", targets); err != nil {
		t.Fatalf("legacy adapter provenance is not signed by trusted targets keys: %v", err)
	}
	data, err := os.ReadFile("../../tuf/repository/targets/releases/49b5a7150ed89099beba0e047e097f56018016116dad4303c016c507110ba977.current.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = targets.Signed.Targets["releases/current.json"].VerifyLengthHashes(data); err != nil {
		t.Fatal(err)
	}
	var release struct {
		AppImage     string `json:"app_image"`
		SourceCommit string `json:"source_commit"`
		Version      string `json:"version"`
	}
	if err = json.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	if release.AppImage != legacyRecoveryAppImage || release.SourceCommit != legacyRecoverySourceCommit || release.Version != "3.1.0" {
		t.Fatal("legacy adapter does not match signed Core source identity")
	}
	for _, candidate := range []string{release.AppImage, release.AppImage + "x", "ghcr.io/another/app@sha256:94c01bfd52941cf48ad9693fce30fc571c29353189fb5e128d0e0752daa36448"} {
		config := instance.Config{Version: "3.1.0", EnvironmentFile: filepath.Join(t.TempDir(), "release.env")}
		if err = os.WriteFile(config.EnvironmentFile, []byte("GEOFLOW_VERSION=3.1.0\nGEOFLOW_APP_IMAGE="+candidate+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		selected, err := legacyAdapterRequired(config)
		if err != nil {
			t.Fatal(err)
		}
		if selected != (candidate == release.AppImage) {
			t.Fatalf("adapter selected incorrect build %s", candidate)
		}
	}
}

func TestLegacyRecoveryUsesFixedAdapterAndGuardAfterRestoreBegins(t *testing.T) {
	service, config, _, _ := recoveryTopologyFixture(t)
	config.Version = "3.1.0"
	if err := os.WriteFile(config.EnvironmentFile, []byte("GEOFLOW_VERSION=3.1.0\nGEOFLOW_APP_IMAGE="+legacyRecoveryAppImage+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	control := service.recoveryControl()
	if _, err := control.Initialize(config.ID); err != nil {
		t.Fatal(err)
	}
	var command string
	service.Runner = functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
		command = strings.Join(args, " ")
		_, err := io.WriteString(out, `{"schema_version":1,"status":"pass","admin_digest":"`+strings.Repeat("a", 64)+`","theme_revisions":0}`)
		return err
	})
	if _, err := service.recoveryPhase(context.Background(), config, "inspect", "recovery-operation", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "init /run/geoflow-recovery-adapter/legacy-recovery.php --phase=inspect") || strings.Contains(command, "artisan geoflow:recovery") {
		t.Fatalf("legacy recovery did not select the fixed signed-build adapter: %s", command)
	}
	path := filepath.Join(filepath.Dir(config.ComposeFile), "recovery-runtime.yml")
	ready, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ready), ":/run/geoflow-recovery-adapter:ro") || strings.Contains(string(ready), "conf.d/zz-geoflow-recovery.ini") {
		t.Fatalf("ready legacy runtime must support inspection without restricting normal operation: %s", ready)
	}
	if _, err := control.Begin(config.ID, "20260827T123456Z-1234abcd", "recovery-operation", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.runtimeArguments(config); err != nil {
		t.Fatal(err)
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(held), ":/usr/local/etc/php/conf.d/zz-geoflow-recovery.ini:ro") != strings.Count(string(held), "GEOFLOW_RECOVERY_CONTRACT:") {
		t.Fatalf("every PHP role must enforce the pinned guard after restore starts: %s", held)
	}
}
