package tufrepo_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/yaojingang/geoflow-updater/internal/tufrepo"
	"gopkg.in/yaml.v3"
)

func TestPublicationResumeExecutesVerifiedSchemaThreePlanPreflight(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["preflight"].Steps {
		if step.Name == "Detect a safe publication resume" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("publication resume preflight is missing")
	}
	for _, scenario := range []struct {
		name, strategy string
		protocol       int
		wantResume     bool
	}{
		{"maintenance-protocol-three", "maintenance", 3, true},
		{"online-protocol-four", "online", 4, true},
		{"online-protocol-too-low", "online", 3, false},
		{"maintenance-legacy-protocol", "maintenance", 2, false},
		{"plan-reference-missing", "maintenance", 3, false},
		{"plan-reference-wrong-version", "maintenance", 3, false},
		{"signed-plan-target-missing", "maintenance", 3, false},
		{"hashed-plan-file-missing", "maintenance", 3, false},
		{"plan-content-tampered-same-length", "maintenance", 3, false},
		{"plan-length-mismatch", "maintenance", 3, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			targets := filepath.Join(root, "source")
			repository := filepath.Join(root, "tuf", "repository")
			mustWriteReleaseManifest(t, targets, 18, "2.4.1", "1", "2")
			manifestPath := filepath.Join(targets, "releases", "current.json")
			manifestBytes, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest["minimum_updater_protocol"] = scenario.protocol
			plan := testUpgradePlan
			if scenario.strategy == "online" {
				plan = strings.Replace(plan, `"strategy":"maintenance"`, `"strategy":"online"`, 1)
				plan = strings.Replace(plan, `"allowed_sources":[]`, `"allowed_sources":[17]`, 1)
				plan = strings.ReplaceAll(plan, "false", "true")
			}
			planTarget := "releases/2.4.1/upgrade-plan.json"
			planSource := filepath.Join(targets, filepath.FromSlash(planTarget))
			mustWrite(t, planSource, []byte(plan))
			switch scenario.name {
			case "plan-reference-missing":
				delete(manifest, "upgrade_plan_target")
			case "plan-reference-wrong-version":
				manifest["upgrade_plan_target"] = "releases/2.4.0/upgrade-plan.json"
				mustWrite(t, filepath.Join(targets, "releases", "2.4.0", "upgrade-plan.json"), []byte(plan))
			case "signed-plan-target-missing":
				if err := os.Remove(planSource); err != nil {
					t.Fatal(err)
				}
			}
			manifestBytes, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, manifestPath, manifestBytes)
			for _, artifact := range []string{"geoflow-updater_0.4.0_linux_amd64.tar.gz", "geoflow-updater_0.4.0_linux_arm64.tar.gz", "checksums.txt"} {
				mustWrite(t, filepath.Join(targets, "updater", "0.4.0", artifact), []byte("fixture artifact"))
			}
			if err := tufrepo.Initialize(tufrepo.InitializeOptions{KeysDir: filepath.Join(root, "keys"), RepositoryDir: repository, TargetsDir: targets}); err != nil {
				t.Fatal(err)
			}
			targetsMetadataPath := filepath.Join(repository, "metadata", "1.targets.json")
			targetsMetadata, err := metadata.Targets().FromFile(targetsMetadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if info := targetsMetadata.Signed.Targets[planTarget]; info != nil {
				planPath := filepath.Join(repository, "targets", "releases", "2.4.1", hex.EncodeToString(info.Hashes["sha256"])+".upgrade-plan.json")
				switch scenario.name {
				case "hashed-plan-file-missing":
					if err := os.Remove(planPath); err != nil {
						t.Fatal(err)
					}
					// An unversioned copy must never satisfy a signed consistent target lookup.
					mustWrite(t, filepath.Join(repository, "targets", filepath.FromSlash(planTarget)), []byte(plan))
				case "plan-content-tampered-same-length":
					tampered := strings.Replace(plan, `"timeout_seconds":600`, `"timeout_seconds":601`, 1)
					if tampered == plan || len(tampered) != len(plan) {
						t.Fatal("tampering fixture must alter bytes without changing length")
					}
					mustWrite(t, planPath, []byte(tampered))
				case "plan-length-mismatch":
					info.Length++
					data, err := targetsMetadata.ToBytes(true)
					if err != nil {
						t.Fatal(err)
					}
					mustWrite(t, targetsMetadataPath, data)
				}
			}
			bin := filepath.Join(root, "bin")
			mustWrite(t, filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 0\n"))
			if err := os.Chmod(filepath.Join(bin, "gh"), 0700); err != nil {
				t.Fatal(err)
			}
			result := filepath.Join(root, "output")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_OUTPUT="+result, "UPDATER_VERSION=0.4.0", "GEOFLOW_VERSION=2.4.1", "RELEASE_SEQUENCE=18", "GEOFLOW_SOURCE_COMMIT="+strings.Repeat("b", 40))
			output, runErr := command.CombinedOutput()
			resultBytes, readErr := os.ReadFile(result)
			if scenario.wantResume {
				if runErr != nil || readErr != nil || string(resultBytes) != "resume=true\n" {
					t.Fatalf("valid planned release cannot resume: err=%v output=%s result=%q", runErr, output, resultBytes)
				}
			} else if runErr == nil || strings.Contains(string(resultBytes), "resume=true") {
				t.Fatalf("invalid plan authorized resume: err=%v output=%s result=%q", runErr, output, resultBytes)
			}
		})
	}
}
