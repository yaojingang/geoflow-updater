package deployment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

func TestDockerIngressServiceStartupWaitsForHealth(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise real startup health")
	}
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("healthy=%t", healthy), func(t *testing.T) {
			root := canonicalTemp(t)
			config := instance.Config{Root: root, ComposeFile: filepath.Join(root, "compose.yml"), EnvironmentFile: filepath.Join(root, "release.env")}
			project := fmt.Sprintf("geoflow-startup-test-%d", time.Now().UnixNano())
			command := "sleep 2; touch /tmp/ready; exec sleep 120"
			if !healthy {
				command = "exec sleep 120"
			}
			compose := fmt.Sprintf(`name: %s
services:
  app:
    image: python:3.12-alpine
    network_mode: none
    command: ["sh", "-c", "%s"]
    healthcheck:
      test: ["CMD-SHELL", "test -f /tmp/ready"]
      interval: 1s
      timeout: 1s
      start_period: 3s
      retries: 1
`, project, command)
			writeTest(t, config.ComposeFile, []byte(compose))
			writeTest(t, config.EnvironmentFile, nil)
			writeTest(t, filepath.Join(root, ".env.prod"), nil)
			arguments := composeArguments(root, config.EnvironmentFile, config.ComposeFile)
			t.Cleanup(func() { _ = exec.Command("docker", append(arguments, "down", "--remove-orphans")...).Run() })
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			service := Service{}
			err := service.startServices(ctx, config, "app")
			if !healthy {
				if err == nil {
					t.Fatal("startup accepted a container whose health check never passes")
				}
				if ctx.Err() != nil {
					t.Fatalf("startup waited until timeout instead of rejecting unhealthy service: %v", err)
				}
				ids, inspectErr := service.containerIDs(ctx, config, "app")
				if inspectErr != nil || len(ids) != 1 {
					t.Fatalf("unhealthy app container: %v, %v (startup: %v)", ids, inspectErr, err)
				}
				if state := dockerRuntime(t, "inspect", "--format={{.State.Health.Status}}", ids[0]); state != "unhealthy" {
					t.Fatalf("startup failed before the health failure: %s, %v", state, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ids, err := service.containerIDs(ctx, config, "app")
			if err != nil || len(ids) != 1 {
				t.Fatalf("app container: %v, %v", ids, err)
			}
			state := dockerRuntime(t, "inspect", "--format={{.State.Health.Status}}", ids[0])
			if strings.TrimSpace(state) != "healthy" {
				t.Fatalf("startup returned before Docker readiness: %s", state)
			}
		})
	}
}
