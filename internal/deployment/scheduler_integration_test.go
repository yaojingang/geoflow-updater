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

func TestDockerLegacySchedulerDrainsChildrenAndStopsIdlePIDOne(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise real Docker process signals")
	}
	root := canonicalTemp(t)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(root, "compose.yml"), EnvironmentFile: filepath.Join(root, "release.env")}
	writeTest(t, filepath.Join(root, ".env.prod"), nil)
	writeTest(t, config.EnvironmentFile, nil)
	// Match the legacy Config.Cmd while Python supplies a PID 1 process without
	// a TERM handler and an initially active child. The stopped parent cannot
	// reap its child; draining must recognize the resulting zombie as exited.
	program := "import subprocess,sys,time\nsubprocess.Popen([sys.executable,'-c','import time; time.sleep(2)'])\nprint('ready',flush=True)\nwhile True: time.sleep(1)\n"
	writeTest(t, filepath.Join(root, "php"), []byte(program))
	name := fmt.Sprintf("geoflow-scheduler-test-%d", time.Now().UnixNano())
	document := fmt.Sprintf("name: %s\nservices:\n  scheduler:\n    image: python:3.12-alpine\n    network_mode: none\n    entrypoint: [python]\n    command: [php, artisan, 'schedule:work']\n    working_dir: /probe\n    volumes: [%q]\n", name, root+":/probe:ro")
	writeTest(t, config.ComposeFile, []byte(document))
	compose := append(composeArguments(root, config.EnvironmentFile, config.ComposeFile), "up", "-d", "scheduler")
	t.Cleanup(func() {
		_ = exec.Command("docker", append(composeArguments(root, config.EnvironmentFile, config.ComposeFile), "down", "--remove-orphans")...).Run()
	})
	if out, err := exec.Command("docker", compose...).CombinedOutput(); err != nil {
		t.Fatalf("start isolated scheduler: %v\n%s", err, out)
	}
	service := Service{StateDir: canonicalTemp(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		out, err := service.output(ctx, config, "logs", "--no-log-prefix", "scheduler")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "ready") {
			break
		}
		if err := waitContext(ctx, 50*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.drainScheduler(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := service.drainScheduler(ctx, config); err != nil {
		t.Fatalf("repeat drain: %v", err)
	}
	ids, err := service.containerIDs(ctx, config, "scheduler")
	if err != nil || len(ids) != 1 {
		t.Fatalf("scheduler identity: %v %v", ids, err)
	}
	state, err := exec.Command("docker", "inspect", "--format={{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}", ids[0]).Output()
	if err != nil || strings.TrimSpace(string(state)) != "false|137|false" {
		t.Fatalf("idle scheduler state: %s %v", state, err)
	}
}
