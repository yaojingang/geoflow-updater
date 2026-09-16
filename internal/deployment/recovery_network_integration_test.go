package deployment

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
)

func TestDockerIngressRecoveryCanRecreateSlotNetworks(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise recovery network reuse")
	}
	service, legacy, candidate, tx := recoveryTopologyFixture(t)
	inspection := filepath.Join(canonicalTemp(t), "artisan")
	writeTest(t, inspection, []byte(`<?php
if (!in_array('geoflow:recovery', $argv, true) || !in_array('--phase=inspect', $argv, true)) { exit(2); }
$state = json_decode(file_get_contents('/run/geoflow-recovery-control/state.json'), true);
if (($state['phase'] ?? null) !== 'ready' || ($state['minimum_updater_protocol'] ?? null) !== 5) { exit(3); }
echo '`+recoveryInspectFixture+`';
`))
	if err := os.Chmod(inspection, 0644); err != nil {
		t.Fatal(err)
	}
	inspections := 0
	service.Runner = functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		if isRecoveryInspect(args) {
			inspections++
		}
		return (RealRunner{}).Run(ctx, in, out, name, args...)
	})
	prefix := fmt.Sprintf("geoflow-recovery-test-%d", time.Now().UnixNano())
	infra := infrastructureConfig(candidate)
	writeTest(t, infra.ComposeFile, []byte(fmt.Sprintf(`name: %s-infra
services:
  probe:
    image: python:3.12-alpine
    command: ["sleep", "600"]
    networks: [data]
networks:
  data:
    name: %s-data
    ipam:
      config:
        - subnet: 198.19.%d.0/28
`, prefix, prefix, time.Now().UnixNano()%250)))
	other := candidate
	other.ActiveSlot = "green"
	other.ComposeFile = filepath.Join(service.instanceDirectory(legacy.ID), "slots", "green", "docker-compose.yml")
	other.EnvironmentFile = filepath.Join(filepath.Dir(other.ComposeFile), "release.env")
	slots := []instance.Config{candidate, other}
	for _, slot := range slots {
		writeTest(t, slot.ComposeFile, []byte(fmt.Sprintf(`name: %s-%s
services:
  init:
    image: php:8.4-cli
    working_dir: /fixture
    volumes:
      - %s:/fixture/artisan:ro
    networks: [data]
  app:
    image: python:3.12-alpine
    command: ["sleep", "600"]
    networks: [data]
networks:
  data:
    name: %s-data
    external: true
`, prefix, slot.ActiveSlot, inspection, prefix)))
		writeTest(t, slot.EnvironmentFile, nil)
	}
	t.Cleanup(func() {
		for _, config := range append(slots, infra) {
			_ = exec.Command("docker", append(composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile), "down", "--remove-orphans")...).Run()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := func() {
		t.Helper()
		if err := service.command(ctx, infra, "up", "-d"); err != nil {
			t.Fatal(err)
		}
		for _, slot := range slots {
			if err := service.startServices(ctx, slot, "app"); err != nil {
				t.Fatalf("slot could not reuse recreated infrastructure network: %v", err)
			}
		}
	}
	service.Recoveries = &topologyRecoveryStore{point: recovery.Point{ID: tx.RecoveryPointID, Deployment: &legacy}, restore: func() error { return nil }}
	start()
	for attempt := 0; attempt < 2; attempt++ {
		old := map[string]string{}
		for _, slot := range slots {
			ids, err := service.output(ctx, slot, "ps", "--all", "--quiet", "app")
			if err != nil || strings.TrimSpace(ids) == "" {
				t.Fatalf("missing initial container: %s %v", ids, err)
			}
			old[slot.ActiveSlot] = strings.TrimSpace(ids)
			if err := service.command(ctx, slot, "stop", "--timeout", "2"); err != nil {
				t.Fatal(err)
			}
		}
		if err := service.saveTransaction(&tx); err != nil {
			t.Fatal(err)
		}
		if err := service.Rollback(ctx, legacy.ID, recovery.RestoreRequest{PointID: tx.RecoveryPointID, TransactionID: "test-restore-transaction"}); err != nil {
			t.Fatal(err)
		}
		start()
		for _, slot := range slots {
			ids, err := service.output(ctx, slot, "ps", "--all", "--quiet", "app")
			if err != nil || strings.TrimSpace(ids) == old[slot.ActiveSlot] {
				t.Fatalf("discarded slot container survived restoration: %s %v", ids, err)
			}
		}
	}
	if inspections != 2 {
		t.Fatalf("expected both restores to inspect the mounted recovery contract, got %d", inspections)
	}
}
