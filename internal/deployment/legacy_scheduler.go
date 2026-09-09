package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

type schedulerState struct {
	Running   bool
	ExitCode  int
	OOMKilled bool
	Pid       int
	StartedAt string
}

// Freeze the legacy PID 1 spawner while its scheduled tasks finish. Laravel's
// schedule:work has no TERM handler; terminate only after proving it is idle.
func (service *Service) drainScheduler(ctx context.Context, config instance.Config) error {
	ids, err := service.containerIDs(ctx, config, "scheduler")
	if err != nil {
		return err
	}
	var other []string
	for _, id := range ids {
		var command limitedBuffer
		if err := service.runner().Run(ctx, nil, &command, "docker", "inspect", "--format={{json .Config.Cmd}}", id); err != nil {
			return err
		}
		var arguments []string
		if err := json.Unmarshal([]byte(command.String()), &arguments); err != nil {
			return err
		}
		if len(arguments) != 3 || arguments[0] != "php" || arguments[1] != "artisan" || arguments[2] != "schedule:work" {
			other = append(other, id)
			continue
		}
		if err := service.drainLegacyScheduler(ctx, config, id); err != nil {
			return err
		}
	}
	return service.drainContainers(ctx, other)
}

func (service *Service) drainLegacyScheduler(ctx context.Context, config instance.Config, id string) (resultErr error) {
	var output limitedBuffer
	if err := service.runner().Run(ctx, nil, &output, "docker", "inspect", "--format={{json .State}}", id); err != nil {
		return err
	}
	var state schedulerState
	if err := json.Unmarshal([]byte(output.String()), &state); err != nil {
		return err
	}
	if state.OOMKilled || state.StartedAt == "" {
		return errors.New("legacy scheduler state is invalid or OOM killed")
	}
	proof := filepath.Join(service.instanceDirectory(config.ID), "legacy-scheduler-drain-"+id+".json")
	if !state.Running {
		if state.ExitCode == 0 || state.ExitCode == 143 {
			return nil
		}
		// Accept our own proven-idle termination after restart, tied to the same
		// container start so an older proof cannot hide a later abnormal exit.
		if state.ExitCode == 137 && regularFile(proof) == nil {
			data, err := os.ReadFile(proof)
			var recorded schedulerState
			if err == nil && json.Unmarshal(data, &recorded) == nil && recorded.Running && recorded.Pid > 0 && !recorded.OOMKilled && recorded.StartedAt == state.StartedAt {
				return nil
			}
		}
		return fmt.Errorf("legacy scheduler did not exit cleanly (exit %d)", state.ExitCode)
	}
	if state.Pid < 1 {
		return errors.New("legacy scheduler parent PID is invalid")
	}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", "update", "--restart=no", id); err != nil {
		return err
	}
	if err := safeDirectory(service.instanceDirectory(config.ID), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	intent := filepath.Join(service.instanceDirectory(config.ID), "legacy-scheduler-freeze-"+id+".json")
	if err := replaceContents(intent, data, 0600); err != nil {
		return err
	}
	defer func() {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, service.thawLegacyScheduler(recovery, id, intent, proof))
	}()
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", "kill", "--signal=STOP", id); err != nil {
		return err
	}
	for {
		var processes limitedBuffer
		if err := service.runner().Run(ctx, nil, &processes, "docker", "top", id, "-eo", "pid,stat"); err != nil {
			return err
		}
		idle, err := legacySchedulerIdle(processes.String(), state.Pid)
		if err != nil {
			return err
		}
		if idle {
			break
		}
		if err := waitContext(ctx, time.Second); err != nil {
			return fmt.Errorf("legacy scheduler still has child tasks: %w", err)
		}
	}
	if err := replaceContents(proof, data, 0600); err != nil {
		return err
	}
	return service.drainContainersSignal(ctx, []string{id}, "KILL")
}

func (service *Service) resumeFrozenSchedulers(ctx context.Context, config instance.Config) error {
	// Only examine schedulers belonging to the deployment being resumed.
	intents, err := filepath.Glob(filepath.Join(service.instanceDirectory(config.ID), "legacy-scheduler-freeze-*.json"))
	if err != nil || len(intents) == 0 {
		return err
	}
	ids, err := service.containerIDs(ctx, config, "scheduler")
	if err != nil {
		return err
	}
	for _, id := range ids {
		intent := filepath.Join(service.instanceDirectory(config.ID), "legacy-scheduler-freeze-"+id+".json")
		if _, err := os.Lstat(intent); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		proof := filepath.Join(service.instanceDirectory(config.ID), "legacy-scheduler-drain-"+id+".json")
		if err := service.thawLegacyScheduler(ctx, id, intent, proof); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) thawLegacyScheduler(ctx context.Context, id, intent, proof string) error {
	if err := regularFile(intent); err != nil {
		return err
	}
	data, err := os.ReadFile(intent)
	if err != nil {
		return err
	}
	var recorded schedulerState
	if len(data) > 4096 || json.Unmarshal(data, &recorded) != nil || !recorded.Running || recorded.Pid < 1 || recorded.StartedAt == "" || recorded.OOMKilled {
		return errors.New("invalid scheduler freeze intent")
	}
	var output limitedBuffer
	if err := service.runner().Run(ctx, nil, &output, "docker", "inspect", "--format={{json .State}}", id); err != nil {
		return err
	}
	var current schedulerState
	if err := json.Unmarshal([]byte(output.String()), &current); err != nil {
		return err
	}
	if current.Running {
		// Revoke any idle termination proof durably before this process can spawn
		// tasks again. A failed KILL must not authorize a future abnormal exit.
		if err := replaceContents(proof, []byte("{}"), 0600); err != nil {
			return err
		}
		if current.StartedAt == recorded.StartedAt && current.Pid == recorded.Pid {
			if err := service.runner().Run(ctx, nil, io.Discard, "docker", "kill", "--signal=CONT", id); err != nil {
				return err
			}
		}
	}
	return os.Remove(intent)
}

func legacySchedulerIdle(processes string, parent int) (bool, error) {
	lines := strings.Split(strings.TrimSpace(processes), "\n")
	if len(lines) < 2 || strings.Join(strings.Fields(lines[0]), " ") != "PID STAT" {
		return false, errors.New("cannot establish legacy scheduler process state")
	}
	found, idle := false, true
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] == "" {
			return false, errors.New("invalid scheduler process row")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid < 1 {
			return false, errors.New("invalid scheduler process PID")
		}
		if pid == parent {
			if found || !strings.HasPrefix(fields[1], "T") {
				return false, errors.New("legacy scheduler spawner is not frozen")
			}
			found = true
		} else if !strings.HasPrefix(fields[1], "Z") {
			idle = false
		}
	}
	if !found {
		return false, errors.New("legacy scheduler spawner is missing")
	}
	return idle, nil
}
