package tufrepo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMetadataPublishingWorkflowsExplicitlyDeployPagesAfterBotCommits(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	for _, name := range []string{"release.yml", "metadata-refresh.yml", "targets-refresh.yml"} {
		contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		workflow := string(contents)
		if !strings.Contains(workflow, "actions: write") {
			t.Errorf("%s does not grant permission to dispatch the Pages workflow", name)
		}
		if !strings.Contains(workflow, "gh workflow run pages.yml --ref main") {
			t.Errorf("%s does not explicitly deploy committed metadata to Pages", name)
		}
	}

	pages, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "pages.yml"))
	if err != nil {
		t.Fatalf("read pages workflow: %v", err)
	}
	if !strings.Contains(string(pages), "workflow_dispatch:") {
		t.Fatal("Pages workflow cannot be dispatched after metadata publication")
	}
	if strings.Contains(string(pages), "push:") {
		t.Fatal("Pages workflow can expose metadata before the release workflow completes validation")
	}
}

func TestReleaseWorkflowsAreValidYAML(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"release-candidate.yml", "release.yml"} {
		contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var document any
		if err := yaml.Unmarshal(contents, &document); err != nil {
			t.Errorf("parse %s: %v", name, err)
		}
	}
}

func TestManagedComposeMountsUpdaterControlCapabilityOnlyIntoTheWebApplication(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", "assets", "docker-compose.managed.yml"))
	if err != nil {
		t.Fatalf("read managed Compose template: %v", err)
	}
	var document struct {
		ApplicationBase struct {
			Environment map[string]string `yaml:"environment"`
			Volumes     []any             `yaml:"volumes"`
			GroupAdd    []string          `yaml:"group_add"`
		} `yaml:"x-app-base"`
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Volumes     []any             `yaml:"volumes"`
			GroupAdd    []string          `yaml:"group_add"`
			Command     []any             `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode managed Compose template: %v", err)
	}
	if _, exposed := document.ApplicationBase.Environment["GEOFLOW_UPDATER_CONTROL_TOKEN_FILE"]; exposed || len(document.ApplicationBase.GroupAdd) != 0 {
		t.Fatal("shared application service base exposes updater mutation capability")
	}
	for serviceName, service := range document.Services {
		_, hasCredential := service.Environment["GEOFLOW_UPDATER_CONTROL_TOKEN_FILE"]
		if serviceName == "app" {
			if !hasCredential || len(service.GroupAdd) != 1 || !containsSubstring(service.Volumes, "control.token") {
				t.Fatal("web application is missing its updater bridge capability")
			}
			continue
		}
		if hasCredential || len(service.GroupAdd) != 0 || containsSubstring(service.Volumes, "control.token") {
			t.Fatalf("service %s unexpectedly receives updater mutation capability", serviceName)
		}
	}
	if _, exists := document.Services["system-update-queue"]; exists {
		t.Fatal("managed Compose still includes the retired application update executor")
	}
	queue := document.Services["queue"]
	if !containsSubstring(queue.Command, "--queue=system-updates,") {
		t.Fatal("managed queue does not consume tombstone system update jobs during Phase C")
	}
}

func containsSubstring(values []any, expected string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.Contains(text, expected) {
			return true
		}
	}

	return false
}

func TestReleaseWorkflowBootstrapsAnEmptyRepositoryAndPinsGEOFlowSource(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(contents)
	for _, required := range []string{
		"current_sequence=0",
		`if jq -e '.signed.targets["releases/current.json"] != null'`,
		"geoflow_sha: ${{ steps.source.outputs.sha }}",
		"ref: ${{ needs.preflight.outputs.geoflow_sha }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing bootstrap or source pin control %q", required)
		}
	}
	candidateContents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release-candidate.yml"))
	if err != nil {
		t.Fatalf("read candidate workflow: %v", err)
	}
	if !strings.Contains(string(candidateContents), "source_commit:$source_commit") {
		t.Error("candidate workflow does not bind the resolved GEOFlow source commit")
	}
}

func TestReleaseWorkflowPublishesOnlyAnApprovedExactCandidate(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", ".github", "workflows")
	contents, err := os.ReadFile(filepath.Join(root, "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(contents)
	for _, required := range []string{
		"candidate_run_id:",
		"phase_c_evidence_sha256:",
		"PHASE_C_REHEARSAL_EVIDENCE_B64",
		"gh run download",
		"phase-c-candidate-$CANDIDATE_RUN_ID",
		`--jq '.path')" = ".github/workflows/release-candidate.yml"`,
		"candidate/targets-source",
		".candidate == $candidate[0]",
		"--draft",
		"--clobber",
		"needs.preflight.outputs.resume",
		"docker buildx imagetools create",
		"verify-bootstrap",
		"signed.targets[$target]",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing candidate publication control %q", required)
		}
	}
	for _, forbidden := range []string{"docker/build-push-action", "go build -trimpath", "needs.images", "--targets-dir \"$targets_dir\""} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("release workflow rebuilds or detaches the approved candidate through %q", forbidden)
		}
	}

	candidateContents, err := os.ReadFile(filepath.Join(root, "release-candidate.yml"))
	if err != nil {
		t.Fatalf("read candidate workflow: %v", err)
	}
	candidate := string(candidateContents)
	for _, required := range []string{
		"candidate-${{ github.run_id }}-${{ github.run_attempt }}",
		"schema_version:2",
		"minimum_updater_protocol:2",
		"candidate/tuf/repository",
		"phase-c-candidate-${{ github.run_id }}",
	} {
		if !strings.Contains(candidate, required) {
			t.Errorf("candidate workflow is missing rehearsal identity control %q", required)
		}
	}
	updaterMain, err := os.ReadFile(filepath.Join("..", "..", "cmd", "geoflow-updater", "main.go"))
	if err != nil {
		t.Fatalf("read updater main: %v", err)
	}
	if !strings.Contains(string(updaterMain), "GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY") {
		t.Fatal("release binary has no explicit root-only candidate repository opt-in")
	}
}
