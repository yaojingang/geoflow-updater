package deployment

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestEdgeNetworkOwnershipAndIdempotency(t *testing.T) {
	for _, scenario := range []struct {
		name, owner                               string
		exists, queryFails, wantError, wantCreate bool
	}{
		{name: "absent", wantCreate: true},
		{name: "owned", exists: true, owner: "geoflow-primary-infra|edge"},
		{name: "foreign", exists: true, owner: "another-site|edge", wantError: true},
		{name: "wrong-role", exists: true, owner: "geoflow-primary-infra|data", wantError: true},
		{name: "engine-unavailable", queryFails: true, wantError: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			created := false
			service := Service{Runner: functionRunner(func(_ context.Context, _ io.Reader, output io.Writer, name string, args ...string) error {
				if name != "docker" || len(args) < 2 || args[0] != "network" {
					t.Fatalf("unexpected command: %s %v", name, args)
				}
				switch args[1] {
				case "ls":
					if scenario.queryFails {
						return errors.New("daemon unavailable")
					}
					if scenario.exists {
						_, _ = io.WriteString(output, "0123456789ab\n")
					}
				case "inspect":
					_, _ = io.WriteString(output, scenario.owner+"\n")
				case "create":
					created = true
					joined := strings.Join(args, " ")
					for _, expected := range []string{"com.docker.compose.project=geoflow-primary-infra", "com.docker.compose.network=edge", "geoflow-primary-edge"} {
						if !strings.Contains(joined, expected) {
							t.Errorf("network ownership missing: %s", joined)
						}
					}
				default:
					t.Fatalf("unexpected network mutation: %v", args)
				}
				return nil
			})}
			if err := service.ensureEdgeNetwork(context.Background(), "primary"); (err != nil) != scenario.wantError {
				t.Fatalf("network result: %v", err)
			}
			if created != scenario.wantCreate {
				t.Fatalf("created = %v", created)
			}
		})
	}
}
