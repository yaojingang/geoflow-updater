package tufrepo_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/operation"
)

func TestRehearsalAcceptsClearedRecoveryCounter(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "phase-c-rehearsal.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	const anchor = "wait_for_operation recovered-after-fault rolled_back update\n"
	first := strings.Index(source, anchor)
	if first < 0 {
		t.Fatal("missing completed-recovery boundary")
	}
	tail := source[first+len(anchor):]
	last := strings.Index(tail, "\nrecord_check persistent-recovery ")
	if last < 0 {
		t.Fatal("missing persistent-recovery check boundary")
	}
	assertions := tail[:last]
	for _, scenario := range []struct {
		name     string
		counter  json.RawMessage
		accepted bool
	}{
		{name: "serialized-zero-omitted", accepted: true},
		{name: "explicit-zero", counter: json.RawMessage(`0`), accepted: true},
		{name: "still-retrying", counter: json.RawMessage(`1`)},
		{name: "negative", counter: json.RawMessage(`-1`)},
		{name: "null", counter: json.RawMessage(`null`)},
		{name: "string-zero", counter: json.RawMessage(`"0"`)},
		{name: "boolean-false", counter: json.RawMessage(`false`)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Use the actual operation wire format, including its zero-value omission.
			completed := time.Date(2026, time.September, 4, 0, 1, 0, 0, time.UTC)
			serialized, err := json.Marshal(operation.Operation{
				SchemaVersion: 1, ID: "fixture", InstanceID: "primary",
				Kind: operation.KindUpdate, Status: operation.StatusRolledBack,
				CurrentStage: "reconciled", CompletedAt: &completed,
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario.counter != nil {
				var record map[string]json.RawMessage
				if err := json.Unmarshal(serialized, &record); err != nil {
					t.Fatal(err)
				}
				record["reconcile_attempts"] = scenario.counter
				serialized, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			for name, content := range map[string][]byte{
				"operation-recovered-after-fault.json": serialized,
				"version.json":                         []byte(`{"version":"2.3.0"}`),
			} {
				if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Host restoration is outside this wire-format check; the sentinel proves
			// the real assertions allow or reject progression to that boundary.
			command := exec.CommandContext(ctx, "bash", "-euc", `
evidence_dir=$1
instance_root=$1
assert_restore_fixture() { printf 'restore-validation-reached\n'; }
`+assertions+`
printf 'recovery-assertions-passed\n'
`, "rehearsal-recovery-test", root)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("recovery assertion test timed out")
			}
			if !scenario.accepted {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 || len(output) != 0 {
					t.Fatalf("invalid counter reached host validation: %v; %s", err, output)
				}
				return
			}
			if err != nil || string(output) != "restore-validation-reached\nrecovery-assertions-passed\n" {
				t.Fatalf("cleared recovery counter was rejected: %v; %s", err, output)
			}
		})
	}
}
