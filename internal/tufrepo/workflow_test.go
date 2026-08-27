package tufrepo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}
