package tufrepo_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func releaseWorkflowStep(t *testing.T, job, name string) string {
	t.Helper()
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
	for _, step := range workflow.Jobs[job].Steps {
		if step.Name == name {
			return step.Run
		}
	}
	t.Fatalf("workflow step %s/%s is missing", job, name)
	return ""
}

func TestPublicationCandidateSourceIdentityOnResume(t *testing.T) {
	script := releaseWorkflowStep(t, "publish", "Download the approved candidate")
	for _, scenario := range []struct {
		name, changedPath, resume string
		wantPass                  bool
	}{
		{"first-publication-exact-source", "", "false", true},
		{"resume-after-metadata-commit", "tuf/repository/metadata/timestamp.json", "true", true},
		{"first-publication-rejects-metadata-descendant", "tuf/repository/metadata/timestamp.json", "false", false},
		{"resume-after-snapshot-refresh", "tuf/repository/metadata/20.snapshot.json", "true", true},
		{"resume-after-targets-publication", "tuf/repository/targets/releases/new.current.json", "true", true},
		{"resume-rejects-runtime", "cmd/geoflow-updater/main.go", "true", false},
		{"resume-rejects-runtime-library", "internal/deployment/executor.go", "true", false},
		{"resume-rejects-compose", "assets/docker-compose.managed.yml", "true", false},
		{"resume-rejects-installer", "packaging/scripts/install.sh", "true", false},
		{"resume-rejects-dependencies", "go.sum", "true", false},
		{"resume-rejects-embedded-trust", "tuf/trust.go", "true", false},
		{"resume-rejects-root", "tuf/repository/metadata/root.json", "true", false},
		{"resume-rejects-versioned-root", "tuf/repository/metadata/2.root.json", "true", false},
		{"resume-rejects-release-workflow", ".github/workflows/release.yml", "true", false},
		{"resume-rejects-evidence-code", "scripts/planned-evidence.py", "true", false},
		{"resume-rejects-replaced-metadata", "tuf/repository/metadata/1.targets.json", "true", false},
		{"resume-rejects-replaced-target", "tuf/repository/targets/releases/old.current.json", "true", false},
		{"resume-rejects-deleted-target", "tuf/repository/targets/releases/old.current.json", "true", false},
		{"resume-rejects-target-symlink", "tuf/repository/targets/releases/new.current.json", "true", false},
		{"resume-rejects-renamed-source", "tuf/repository/targets/releases/source.txt", "true", false},
		{"resume-rejects-unrelated-source", "tuf/repository/metadata/timestamp.json", "true", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				command := exec.Command("git", args...)
				command.Dir = root
				command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "--quiet")
			git("config", "user.name", "Release test")
			git("config", "user.email", "release-test@example.invalid")
			mustWrite(t, filepath.Join(root, "source.txt"), []byte("approved source\n"))
			mustWrite(t, filepath.Join(root, "tuf/repository/metadata/1.targets.json"), []byte("immutable metadata\n"))
			mustWrite(t, filepath.Join(root, "tuf/repository/targets/releases/old.current.json"), []byte("immutable target\n"))
			git("add", ".")
			git("commit", "--quiet", "-m", "candidate source")
			candidateSHA := git("rev-parse", "HEAD")
			if scenario.changedPath != "" {
				mustWrite(t, filepath.Join(root, filepath.FromSlash(scenario.changedPath)), []byte("publication change\n"))
				switch scenario.name {
				case "resume-rejects-deleted-target":
					if err := os.Remove(filepath.Join(root, scenario.changedPath)); err != nil {
						t.Fatal(err)
					}
				case "resume-rejects-target-symlink":
					path := filepath.Join(root, scenario.changedPath)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("../../../source.txt", path); err != nil {
						t.Fatal(err)
					}
				case "resume-rejects-renamed-source":
					if err := os.Rename(filepath.Join(root, "source.txt"), filepath.Join(root, scenario.changedPath)); err != nil {
						t.Fatal(err)
					}
				}
				git("add", ".")
				git("commit", "--quiet", "-m", "publication metadata")
			}
			publicationSHA := git("rev-parse", "HEAD")
			if scenario.name == "resume-rejects-unrelated-source" {
				candidateSHA = git("commit-tree", "HEAD^{tree}", "-m", "unrelated candidate")
			}
			bin := filepath.Join(root, "bin")
			mustWrite(t, filepath.Join(bin, "gh"), []byte(`#!/bin/sh
if [ "$1 $2" = 'run download' ]; then exit 0; fi
case "$4" in
  .conclusion) echo success ;;
  .head_sha) echo "$TEST_CANDIDATE_SHA" ;;
  .name) echo 'Build Phase C release candidate' ;;
  .path) echo '.github/workflows/release-candidate.yml' ;;
  .event) echo workflow_dispatch ;;
  *) exit 3 ;;
esac
`))
			if err := os.Chmod(filepath.Join(bin, "gh"), 0700); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(root, "outputs")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY=yaojingang/geoflow-updater", "CANDIDATE_RUN_ID=42", "GITHUB_SHA="+publicationSHA,
				"TEST_CANDIDATE_SHA="+candidateSHA, "PUBLICATION_RESUME="+scenario.resume, "GITHUB_OUTPUT="+outputPath,
				"RUNNER_TEMP="+root)
			output, err := command.CombinedOutput()
			if scenario.wantPass && err != nil {
				t.Fatalf("candidate publication source rejected: %v\n%s", err, output)
			}
			if !scenario.wantPass && err == nil {
				t.Fatalf("changed source authorized publication: %s", output)
			}
			if scenario.wantPass && scenario.resume == "true" {
				outputs, err := os.ReadFile(outputPath)
				if err != nil || !strings.Contains(string(outputs), fmt.Sprintf("candidate_sha=%s\n", candidateSHA)) {
					t.Fatalf("resume lost candidate provenance: %v, outputs=%s", err, outputs)
				}
			}
		})
	}
}

func TestPublicationResumeRequiresEveryCandidateTarget(t *testing.T) {
	script := releaseWorkflowStep(t, "publish", "Verify committed candidate targets before publication resumes")
	bin := t.TempDir()
	verifier := filepath.Join(bin, "geoflow-tuf")
	build := exec.Command("go", "build", "-o", verifier, "./cmd/geoflow-tuf")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build repository verifier: %v\n%s", err, output)
	}
	// Execute the workflow's CLI with a real compiled verifier in an isolated
	// repository fixture. Only the Go compiler invocation is substituted.
	mustWrite(t, filepath.Join(bin, "go"), []byte(`#!/bin/sh
test "$1 $2" = 'run ./cmd/geoflow-tuf' || exit 3
shift 2
exec "$TEST_TUF_VERIFIER" "$@"
`))
	if err := os.Chmod(filepath.Join(bin, "go"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, changedTarget string
		wantPass            bool
	}{
		{"same-candidate", "", true},
		{"different-compose", "deploy/docker-compose.managed.yml", false},
		{"different-version-document", "releases/3.1.0/version.json", false},
		{"different-upgrade-plan", "releases/3.1.0/upgrade-plan.json", false},
		{"different-updater-archive", "updater/0.4.0/geoflow-updater_0.4.0_linux_arm64.tar.gz", false},
		{"different-checksums", "updater/0.4.0/checksums.txt", false},
		{"extra-signed-online-target", "", false},
		{"missing-signed-target", "", false},
		{"bad-root-signature", "", false},
		{"bad-timestamp-signature", "", false},
		{"bad-snapshot-signature", "", false},
		{"bad-targets-signature", "", false},
		{"expired-metadata", "", false},
		{"published-target-tampered", "", false},
		{"published-target-missing", "", false},
		{"published-target-symlink", "", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			candidate := filepath.Join(root, "candidate", "targets-source")
			for _, target := range []string{"releases/current.json", "deploy/docker-compose.managed.yml",
				"releases/3.1.0/version.json", "releases/3.1.0/upgrade-plan.json",
				"updater/0.4.0/geoflow-updater_0.4.0_linux_amd64.tar.gz",
				"updater/0.4.0/geoflow-updater_0.4.0_linux_arm64.tar.gz", "updater/0.4.0/checksums.txt"} {
				contents := []byte("approved candidate " + target)
				mustWrite(t, filepath.Join(candidate, filepath.FromSlash(target)), contents)
				if target == scenario.changedTarget {
					contents = []byte("another candidate " + target)
				}
				mustWrite(t, filepath.Join(source, filepath.FromSlash(target)), contents)
			}
			manifestHash := sha256.Sum256([]byte("approved candidate releases/current.json"))
			mustWrite(t, filepath.Join(root, "candidate/candidate.json"), []byte(fmt.Sprintf(
				`{"targets":{"release_manifest_sha256":"%x"}}`, manifestHash)))
			if scenario.name == "extra-signed-online-target" {
				mustWrite(t, filepath.Join(source, "online-fixture/release.json"), []byte("unapproved online fixture"))
			}
			if scenario.name == "missing-signed-target" {
				if err := os.Remove(filepath.Join(source, "releases/3.1.0/upgrade-plan.json")); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			if scenario.name == "expired-metadata" {
				now = now.Add(-8 * 24 * time.Hour)
			}
			repository := filepath.Join(root, "tuf", "repository")
			if err := tufrepo.Initialize(tufrepo.InitializeOptions{KeysDir: filepath.Join(root, "keys"),
				RepositoryDir: repository, TargetsDir: source, Now: func() time.Time { return now }}); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario.name, "bad-") {
				role := strings.TrimSuffix(strings.TrimPrefix(scenario.name, "bad-"), "-signature")
				name := role + ".json"
				if role == "snapshot" || role == "targets" {
					name = "1." + name
				}
				path := filepath.Join(repository, "metadata", name)
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var envelope map[string]any
				if err := json.Unmarshal(contents, &envelope); err != nil {
					t.Fatal(err)
				}
				for _, entry := range envelope["signatures"].([]any) {
					entry.(map[string]any)["sig"] = strings.Repeat("0", 128)
				}
				contents, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, path, contents)
			}
			if strings.HasPrefix(scenario.name, "published-target-") {
				path := filepath.Join(repository, "targets/releases", fmt.Sprintf("%x.current.json", manifestHash))
				switch scenario.name {
				case "published-target-tampered":
					mustWrite(t, path, []byte("damaged target bytes"))
				case "published-target-missing", "published-target-symlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if scenario.name == "published-target-symlink" {
						if err := os.Symlink(filepath.Join(candidate, "releases/current.json"), path); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_TUF_VERIFIER="+verifier)
			output, err := command.CombinedOutput()
			if scenario.wantPass && err != nil {
				t.Fatalf("same published candidate rejected: %v\n%s", err, output)
			}
			if !scenario.wantPass && err == nil {
				t.Fatalf("different committed target accepted on resume: %s", output)
			}
		})
	}
}

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
		{"draft-hidden-from-read-only-preflight", "maintenance", 3, true},
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
			ghScript := "#!/bin/sh\nexit 0\n"
			if scenario.name == "draft-hidden-from-read-only-preflight" {
				ghScript = "#!/bin/sh\necho 'HTTP 404: draft requires push access' >&2\nexit 1\n"
			}
			mustWrite(t, filepath.Join(bin, "gh"), []byte(ghScript))
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

func TestPublicationResumeChecksReleaseInProtectedJob(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Permissions map[string]string
		Jobs        map[string]struct {
			Environment string
			Permissions map[string]string
			Steps       []struct {
				Name, If, Run string
				Env           map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatal(err)
	}
	publish := workflow.Jobs["publish"]
	if workflow.Jobs["preflight"].Permissions["contents"] != "read" || publish.Environment != "release-signing" || workflow.Permissions["contents"] != "write" || len(publish.Permissions) != 0 {
		t.Fatal("draft access must use the protected publisher's existing write permission")
	}
	var script string
	for _, step := range publish.Steps {
		if step.Name == "Verify existing release for publication resume" {
			if step.If != "needs.preflight.outputs.resume == 'true'" || step.Env["GH_TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" {
				t.Fatal("release visibility check must authenticate only on resume")
			}
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("protected draft check is missing")
	}
	for _, state := range []string{"draft", "published", "missing"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			gh := filepath.Join(root, "gh")
			mustWrite(t, gh, []byte("#!/bin/sh\ntest \"$*\" = 'release view v0.4.0 --repo yaojingang/geoflow-updater' || exit 3\necho checked > \"$TEST_TRACE\"\ntest \"$TEST_RELEASE_STATE\" != missing\n"))
			if err := os.Chmod(gh, 0700); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(root, "trace")
			command := exec.Command("bash", "-euo", "pipefail", "-c", script)
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "UPDATER_VERSION=0.4.0", "GITHUB_REPOSITORY=yaojingang/geoflow-updater", "TEST_TRACE="+trace, "TEST_RELEASE_STATE="+state)
			output, err := command.CombinedOutput()
			if (err == nil) != (state != "missing") {
				t.Fatalf("release state %s: %v, %s", state, err, output)
			}
			if data, err := os.ReadFile(trace); err != nil || string(data) != "checked\n" {
				t.Fatalf("release check did not execute: %v, %s", err, data)
			}
		})
	}
}
