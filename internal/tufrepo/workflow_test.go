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

	for _, name := range []string{"phase-c-rehearsal.yml", "release-candidate.yml", "release.yml"} {
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

func TestPhaseCRehearsalUsesFreshNativeRunnersAndAnExactCandidate(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "phase-c-rehearsal.yml"))
	if err != nil {
		t.Fatalf("read Phase C rehearsal workflow: %v", err)
	}
	workflow := string(contents)
	for _, required := range []string{
		"workflow_dispatch:",
		"candidate_run_id:",
		"ubuntu-24.04",
		"ubuntu-24.04-arm",
		"uname -m",
		"actions/download-artifact",
		"phase-c-candidate-${{ inputs.candidate_run_id }}",
		`actions/runs/$CANDIDATE_RUN_ID`,
		`.github/workflows/release-candidate.yml`,
		`'.triggering_actor.login'`,
		"GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
		"persist-credentials: false",
		`install -d -m 0700 "$GITHUB_WORKSPACE/rehearsal/evidence"`,
		"gh attestation verify",
		"scripts/phase-c-rehearsal.sh",
		"phase-c-rehearsal-${{ matrix.platform }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("Phase C rehearsal workflow is missing native-runner control %q", required)
		}
	}
	for _, forbidden := range []string{"docker/setup-qemu", "docker/setup-buildx"} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("Phase C rehearsal workflow uses emulation through %q", forbidden)
		}
	}
}

func TestPhaseCRehearsalExercisesTheInstalledAgentAndManagedDeployment(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "phase-c-rehearsal.sh"))
	if err != nil {
		t.Fatalf("read Phase C rehearsal script: %v", err)
	}
	script := string(contents)
	for _, required := range []string{
		"go mod download",
		"go test -race ./...",
		"/opt/geoflow-phase-c-rehearsal",
		"packaging/scripts/install.sh",
		"geoflow-updater enroll",
		"geoflow-updater authorization-uri",
		"GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY=1",
		"mutation_authorization_required",
		"mutation_authorization_replayed",
		"retired-update-worker",
		"system-update-queue",
		"queues:system-updates:reserved",
		"queues:system-updates:delayed",
		"website_start_operation system-updates/updater/update",
		"website_start_operation system-updates/updater/backup",
		"website_start_operation system-updates/updater/rollback",
		"website_start_operation system-updates/updater/verify",
		"data-admin-errors",
		"GEOFLOW_UPDATE_REQUIRE_ADMIN_PASSWORD",
		"database.dump",
		"storage.tar.gz",
		"redis.tar.gz",
		"managed/docker-compose.yml",
		"wait_for_operation",
		"recovery-points",
		"forced-migration-failure",
		"forced-activation-failure",
		"restart-during-migration",
		"persistent-recovery",
		"restart-during-resume",
		"migrate-completed",
		"resume-completed",
		"restore_migrations_sha",
		"storage/logs",
		"redact_evidence",
		".phase-c-rehearsal-owned",
		`chown -R "$evidence_owner_uid:$evidence_owner_gid"`,
		`"result":"pass"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("Phase C rehearsal script is missing host-rehearsal control %q", required)
		}
	}
	compatibilityAt := strings.Index(script, "current_check=phase-b-compose-compatibility")
	handoverAt := strings.Index(script, "current_check=managed-phase-b-handover")
	if compatibilityAt < 0 || handoverAt < 0 || compatibilityAt >= handoverAt {
		t.Fatal("Phase B signed Compose compatibility repair must run before managed handover")
	}
	laravelBootstrap := `require "/var/www/html/vendor/autoload.php"; $app = require "/var/www/html/bootstrap/app.php";`
	if got := strings.Count(script, laravelBootstrap); got != 3 {
		t.Fatalf("Phase C Laravel probes with Composer autoload = %d, want 3", got)
	}
	for _, required := range []string{
		`local expected_error_marker=$2`,
		`grep -Fq "$expected_error_marker" "$page"`,
		`website_expect_validation_error "" admin-flash-alert system-updates/updater/update`,
		`website_expect_validation_error "" data-admin-errors system-updates/updater/rollback`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("Phase C rehearsal script is missing version-specific website validation control %q", required)
		}
	}
	phaseBLegacyControlsAt := strings.Index(script, `grep -Fq 'name="updater_authorization_code"'`)
	phaseCWebsiteAt := strings.Index(script, "current_check=website-bridge")
	phaseCAuthorizedActionAt := strings.Index(script, `grep -Fq 'data-system-updater-authorized-action'`)
	if phaseBLegacyControlsAt < 0 || phaseCWebsiteAt < 0 || phaseCAuthorizedActionAt < phaseCWebsiteAt {
		t.Fatal("Website rehearsal must verify legacy Phase B controls before checking the Phase C authorized-action marker")
	}
	for _, required := range []string{
		"a6a8b6ca1f0e7c9c00c4093a237206a6c24131fc94e5540c3b1dfd1fe84dcc67",
		"95383a1b19ee80c7c4b05cfffc0868c6492ab4e5870f173e581e4240ebbcce6f",
		`cmp "$instance_dir/docker-compose.managed.yml" "$updater_root/assets/docker-compose.managed.yml"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("Phase C rehearsal script is missing Phase B compatibility control %q", required)
		}
	}
	for _, forbidden := range []string{"--platform linux/", "docker run --platform"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("Phase C rehearsal script uses architecture emulation through %q", forbidden)
		}
	}

	wrapperContents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "rehearsal-docker-wrapper.sh"))
	if err != nil {
		t.Fatalf("read Phase C Docker fault wrapper: %v", err)
	}
	wrapper := string(wrapperContents)
	for _, required := range []string{"fail-migrate", "fail-activate", "pause-migrate", "pause-resume", "pause-migrate-fail-restore", "corrupt_recovery_surfaces", "/usr/bin/docker"} {
		if !strings.Contains(wrapper, required) {
			t.Errorf("Phase C Docker fault wrapper is missing %q", required)
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

func TestManagedComposeWebHealthcheckExpandsThePrimaryHost(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("..", "..", "assets", "docker-compose.managed.yml"))
	if err != nil {
		t.Fatalf("read managed Compose template: %v", err)
	}
	var document struct {
		Services map[string]struct {
			Healthcheck struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode managed Compose template: %v", err)
	}
	healthcheck := document.Services["web"].Healthcheck.Test
	if len(healthcheck) != 2 {
		t.Fatalf("web healthcheck = %#v, want CMD-SHELL and one command", healthcheck)
	}
	if !strings.Contains(healthcheck[1], `--header="Host: $${GEOFLOW_NGINX_PRIMARY_HOST}"`) {
		t.Fatalf("web healthcheck does not expand its primary Host inside the container: %q", healthcheck[1])
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
		"superadmin_risk_waiver:",
		"superadmin_risk_waiver_acknowledgement:",
		"superadmin_risk_waiver_reason:",
		"PHASE_C_REHEARSAL_EVIDENCE_B64",
		`test "$GITHUB_ACTOR" = "$GITHUB_REPOSITORY_OWNER"`,
		`test "$GITHUB_TRIGGERING_ACTOR" = "$GITHUB_REPOSITORY_OWNER"`,
		"I_ACCEPT_PHASE_C_RELEASE_WITHOUT_DUAL_ARCH_REHEARSAL",
		`test "$reason_non_whitespace" -ge 10`,
		`test -z "$PHASE_C_EVIDENCE_SHA256"`,
		`mode: "superadmin-risk-waiver"`,
		`mode: "dual-architecture-rehearsal"`,
		"publication-authorization.json",
		"Attest publication authorization",
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
