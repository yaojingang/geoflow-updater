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
)

func TestRollbackDiagnosticReportsOnlyBooleanOutcomes(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "phase-c-rehearsal.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var functions strings.Builder
	for _, name := range []string{"diagnostic_value", "diagnostic_expect"} {
		start := strings.Index(string(contents), "\n"+name+"() {\n")
		if start < 0 {
			t.Fatalf("missing diagnostic function %s", name)
		}
		body := string(contents)[start+1:]
		end := strings.Index(body, "\n}\n")
		if end < 0 {
			t.Fatalf("incomplete diagnostic function %s", name)
		}
		functions.WriteString(body[:end+3])
	}
	for _, tc := range []struct {
		name    string
		value   string
		status  string
		ok      bool
		matches bool
	}{
		{"matching", "before-update", "0", true, true},
		{"changed", "synthetic-private-value", "0", true, false},
		{"absent", "", "0", true, false},
		{"failed-read", "before-update", "1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-euc", `
evidence_dir=$1
probe_value=$2
probe_status=$3
probe() {
    printf '%s' "$probe_value"
    printf 'synthetic-private-stderr' >&2
    return "$probe_status"
}
`+functions.String()+`
diagnostic_value marker before-update probe
diagnostic_expect command probe
printf 'diagnostic-recorded\n'
`, "diagnostic-test", root, tc.value, tc.status)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err != nil || string(output) != "diagnostic-recorded\n" {
				t.Fatalf("diagnostic leaked command output or failed: %v", err)
			}
			records, err := os.ReadFile(filepath.Join(root, "rollback-diagnostic.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(records)), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d records, want 2", len(lines))
			}
			for index, line := range lines {
				var got map[string]any
				if err := json.Unmarshal([]byte(line), &got); err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					if len(got) != 3 || got["id"] != "marker" || got["command_ok"] != tc.ok || got["matches_expected"] != tc.matches {
						t.Fatal("value diagnostic did not preserve read/match outcomes")
					}
				} else if len(got) != 2 || got["id"] != "command" || got["pass"] != tc.ok {
					t.Fatal("command diagnostic did not preserve exit outcome")
				}
			}
			for _, forbidden := range []string{"before-update", "synthetic-private-value", "synthetic-private-stderr"} {
				if strings.Contains(string(records), forbidden) {
					t.Fatal("diagnostic included command data")
				}
			}
		})
	}
}
