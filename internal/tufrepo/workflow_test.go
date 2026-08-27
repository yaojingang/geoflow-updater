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
		"source_commit:$source_commit",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing bootstrap or source pin control %q", required)
		}
	}
}

func TestReleaseWorkflowStagesImagesAndUsesRecoverableDraftRelease(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(contents)
	for _, required := range []string{
		"staging-${{ github.run_id }}-${{ github.run_attempt }}",
		"--draft",
		"--clobber",
		"needs.preflight.outputs.resume",
		"docker buildx imagetools create",
		"verify-bootstrap",
		"signed.targets[$target]",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing recoverability control %q", required)
		}
	}
	if strings.Contains(workflow, "ghcr.io/yaojingang/geoflow-app:${{ inputs.geoflow_version }}\n            ghcr.io/yaojingang/geoflow-app:latest") {
		t.Error("release workflow publishes application release tags during the staging build")
	}
}
