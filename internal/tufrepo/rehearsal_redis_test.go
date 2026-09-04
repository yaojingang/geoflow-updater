package tufrepo_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRehearsalRedisBaselineSurvivesManagedHandover(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "phase-c-rehearsal.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	section := func(start, end string) string {
		t.Helper()
		first := strings.Index(source, start)
		if first < 0 {
			t.Fatalf("missing rehearsal section %q", start)
		}
		last := strings.Index(source[first+len(start):], end)
		if last < 0 {
			t.Fatalf("missing rehearsal section end %q", end)
		}
		return source[first : first+len(start)+last]
	}
	var helpers strings.Builder
	for _, name := range []string{"record_check", "read_environment_value"} {
		helpers.WriteString(section(name+"() {\n", "\n}\n") + "\n}\n")
	}
	// Execute the real two lifecycle sections; Docker is the external boundary.
	// Its legacy Redis data disappears at down, while managed data persists.
	scenarios := section("current_check=pre-cutover-idle\n", "current_check=enrollment-boundary\n") +
		section("current_check=managed-phase-b-handover\n", "current_check=authorization-factors\n")
	for _, scenario := range []string{"matching", "write-failure", "read-failure", "wrong-readback"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range map[string]string{
				"release.env": "GEOFLOW_APP_IMAGE=fixture-image\n",
				".env.prod":   "DOCKER_NETWORK_NAME=geoflow-phase-c-rehearsal-net\n",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-euc", `
fixture_root=$1
fixture_scenario=$2
instance_root=$fixture_root
instance_dir=$fixture_root
checks_file=$fixture_root/checks.jsonl
PLATFORM=linux-amd64
candidate_run_id=fixture
redis_password=synthetic-password
docker() {
    case " $* " in
        *" redis-cli SET "*)
            if [[ $fixture_scenario == write-failure ]]; then return 1; fi
            printf '%s\n' "${!#}" > "$fixture_root/redis-marker"
            ;;
        *" redis-cli GET "*)
            if [[ $fixture_scenario == read-failure ]]; then return 1; fi
            if [[ $fixture_scenario == wrong-readback ]]; then printf 'unexpected-value\n'; return 0; fi
            if test -f "$fixture_root/redis-marker"; then
                command cat "$fixture_root/redis-marker"
            fi
            ;;
        *" redis-cli LLEN "*|*" redis-cli ZCARD "*) printf '0\n' ;;
        *" psql "*)
            if [[ $* == *"SELECT count(*)"* ]]; then printf '0\n'; fi
            ;;
        " compose "*)
            for argument in "$@"; do
                if [[ $argument == down ]]; then
                    command rm -f -- "$fixture_root/redis-marker"
                fi
            done
            ;;
        " run "*) : ;;
        " inspect "*) printf 'true\n' ;;
        *) printf 'Unexpected Docker fixture operation\n' >&2; return 2 ;;
    esac
}
`+helpers.String()+scenarios+`
printf 'handover-finished\n'
test "$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli GET "geoflow:${marker}:before")" = before-update
printf 'managed-baseline-ready\n'
`, "rehearsal-redis-test", root, scenario)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("rehearsal lifecycle test timed out")
			}
			if scenario != "matching" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 || len(output) != 0 {
					t.Fatalf("invalid baseline was not rejected inside the real handover: %v; %s", err, output)
				}
				return
			}
			if err != nil || string(output) != "handover-finished\nmanaged-baseline-ready\n" {
				t.Fatalf("managed handover did not establish the Redis recovery baseline: %v; %s", err, output)
			}
		})
	}
}
