package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

const ingressWorkersScript = `for f in /proc/[0-9]*/cmdline; do
  command=$(tr '\000' ' ' < "$f" 2>/dev/null) || continue
  case "$command" in "nginx: worker process"*)
    pid=${f#/proc/}; pid=${pid%/cmdline}
    start=$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null) || continue
    printf '%s:%s\n' "$pid" "$start"
  esac
done`

func (service *Service) ingressWorkers(ctx context.Context, config instance.Config) ([]string, error) {
	out, err := service.output(ctx, infrastructureConfig(config), "exec", "-T", "edge", "sh", "-ec", ingressWorkersScript)
	if err != nil {
		return nil, err
	}
	workers := strings.Fields(out)
	for _, worker := range workers {
		parts := strings.Split(worker, ":")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Trim(parts[0]+parts[1], "0123456789") != "" {
			return nil, errors.New("invalid ingress worker identity")
		}
	}
	return workers, nil
}
func (service *Service) switchIngress(ctx context.Context, config instance.Config, sequence uint64, running bool) ([]string, error) {
	if running {
		output, err := service.output(ctx, infrastructureConfig(config), "ps", "--status", "running", "--quiet", "edge")
		if err != nil {
			return nil, err
		}
		running = strings.TrimSpace(output) != ""
	}
	_, scheme, port, err := siteAuthority(config.Root)
	if err != nil {
		return nil, err
	}
	contents, err := renderIngress(config.ActiveSlot, sequence, scheme, port)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(service.instanceDirectory(config.ID), "infra", "traffic")
	if err := safeDirectory(directory, 0755); err != nil {
		return nil, err
	}
	candidatePath := filepath.Join(directory, "candidate.conf")
	if err := replaceContents(candidatePath, contents, 0644); err != nil {
		return nil, err
	}
	infra := infrastructureConfig(config)
	if running {
		if err := service.command(ctx, infra, "exec", "-T", "edge", "nginx", "-t", "-c", "/etc/geoflow/candidate.conf"); err != nil {
			return nil, err
		}
	} else {
		if err := service.command(ctx, infra, "run", "--rm", "--no-deps", "--entrypoint", "nginx", "edge", "-t", "-c", "/etc/geoflow/candidate.conf"); err != nil {
			return nil, err
		}
	}
	var workers []string
	if running {
		workers, err = service.ingressWorkers(ctx, config)
		if err != nil {
			return nil, err
		}
		if len(workers) == 0 {
			return nil, errors.New("running ingress has no observable workers")
		}
	}
	path := filepath.Join(directory, "nginx.conf")
	old, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if err := replaceContents(path, contents, 0644); err != nil {
		return nil, err
	}
	if running {
		err = service.command(ctx, infra, "exec", "-T", "edge", "nginx", "-s", "reload", "-c", "/etc/geoflow/nginx.conf")
	} else {
		err = service.command(ctx, infra, "up", "-d", "--no-deps", "--wait", "--wait-timeout", "60", "edge")
	}
	if err != nil {
		if len(old) > 0 {
			_ = replaceContents(path, old, 0644)
		}
		return workers, err
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := service.checkIngressIdentity(deadline, config); err == nil {
			return workers, nil
		}
		if err := waitContext(deadline, 250*time.Millisecond); err != nil {
			return workers, errors.New("ingress did not activate the requested release")
		}
	}
}
func (service *Service) checkIngressIdentity(ctx context.Context, config instance.Config) error {
	output, err := service.output(ctx, infrastructureConfig(config), "exec", "-T", "edge", "wget", "-q", "-T", "5", "-O", "-", "http://127.0.0.1:8081/release")
	if err != nil {
		return err
	}
	var current struct {
		Slot     string `json:"slot"`
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(output), &current); err != nil || current.Slot != config.ActiveSlot || current.Sequence != config.ReleaseSequence {
		return errors.New("ingress release does not match managed deployment state")
	}
	return nil
}
func (service *Service) waitIngressWorkers(ctx context.Context, config instance.Config, workers []string) error {
	if len(workers) == 0 {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, 17*time.Minute)
	defer cancel()
	old := map[string]bool{}
	for _, worker := range workers {
		old[worker] = true
	}
	for {
		current, err := service.ingressWorkers(deadline, config)
		if err != nil {
			return err
		}
		remaining := false
		for _, worker := range current {
			if old[worker] {
				remaining = true
			}
		}
		if !remaining {
			return nil
		}
		if err := waitContext(deadline, time.Second); err != nil {
			return errors.New("ingress still serves old in-flight requests; old slot retained")
		}
	}
}
func (service *Service) installInfrastructure(ctx context.Context, source, candidate instance.Config, release managed.Release, fresh ...bool) error {
	_, infra, err := renderTopology(release.ComposeTemplate, candidate, candidate.ActiveSlot, service.instanceDirectory(source.ID))
	if err != nil {
		return err
	}
	directory := filepath.Dir(candidate.InfraComposeFile)
	if err := safeDirectory(filepath.Join(directory, "traffic"), 0755); err != nil {
		return err
	}
	if err := safeDirectory(filepath.Join(service.instanceDirectory(source.ID), "assets"), 0755); err != nil {
		return err
	}
	environment, err := os.ReadFile(candidate.EnvironmentFile)
	if err != nil {
		return err
	}
	if err := replaceContents(candidate.InfraComposeFile, infra, 0600); err != nil {
		return err
	}
	if err := replaceContents(candidate.InfraEnvironmentFile, environment, 0600); err != nil {
		return err
	}
	if err := service.command(ctx, infrastructureConfig(candidate), "config", "--quiet"); err != nil {
		return err
	}
	// Every application writer is already drained and a verified recovery point exists.
	if len(fresh) == 0 || !fresh[0] {
		if err := service.command(ctx, infrastructureConfig(source), "down", "--remove-orphans"); err != nil {
			return err
		}
	}
	if err := service.command(ctx, infrastructureConfig(candidate), "up", "-d", "--wait", "--wait-timeout", "180", "postgres", "redis"); err != nil {
		return err
	}
	// PostgreSQL and Redis create only the data network. Application slots need
	// the external edge network before ingress can be started with a ready slot.
	if err := service.ensureEdgeNetwork(ctx, candidate.ID); err != nil {
		return err
	}
	if source.Layout == LayoutBlueGreen {
		return service.command(ctx, infrastructureConfig(candidate), "up", "-d", "--no-deps", "--wait", "edge")
	}
	return nil
}

func (service *Service) ensureEdgeNetwork(ctx context.Context, id string) error {
	if !instanceIDPattern.MatchString(id) {
		return errors.New("invalid instance for edge network")
	}
	name := "geoflow-" + id + "-edge"
	project := "geoflow-" + id + "-infra"
	var existing limitedBuffer
	if err := service.runner().Run(ctx, nil, &existing, "docker", "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.ID}}"); err != nil {
		return fmt.Errorf("find edge network: %w", err)
	}
	if strings.TrimSpace(existing.String()) == "" {
		return service.runner().Run(ctx, nil, io.Discard, "docker", "network", "create",
			"--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.network=edge", name)
	}
	var owner limitedBuffer
	if err := service.runner().Run(ctx, nil, &owner, "docker", "network", "inspect", "--format",
		`{{index .Labels "com.docker.compose.project"}}|{{index .Labels "com.docker.compose.network"}}`, name); err != nil {
		return fmt.Errorf("inspect edge network ownership: %w", err)
	}
	if strings.TrimSpace(owner.String()) != project+"|edge" {
		return errors.New("edge network belongs to another deployment")
	}
	return nil
}
func (service *Service) restoreApplication(ctx context.Context, tx *releaseTransaction) error {
	if tx.Source.Layout != LayoutBlueGreen {
		return errors.New("application-only rollback requires a retained blue-green source")
	}
	if err := service.drainBackground(ctx, tx.Candidate); err != nil {
		return err
	}
	if err := service.startServices(ctx, tx.Source, "app", "reverb", "web"); err != nil {
		return err
	}
	if err := service.waitHTTP(ctx, tx.Source); err != nil {
		return err
	}
	workers, err := service.switchIngress(ctx, tx.Source, tx.Source.ReleaseSequence, true)
	if err != nil {
		return err
	}
	if err := service.activateSlot(tx.Source, tx.SourceVersion); err != nil {
		return err
	}
	names, err := backgroundServices(tx.Source.ComposeFile)
	if err != nil {
		return err
	}
	if err := service.startServices(ctx, tx.Source, names...); err != nil {
		return err
	}
	if err := service.drainServices(ctx, tx.Candidate, "reverb"); err != nil {
		return err
	}
	if err := service.waitIngressWorkers(ctx, tx.Source, workers); err != nil {
		return err
	}
	return service.drainServices(ctx, tx.Candidate, "web", "app")
}
func (service *Service) restoreBeforeTraffic(ctx context.Context, tx *releaseTransaction) error {
	if tx.LayoutStarted {
		names, err := applicationServices(tx.Candidate.ComposeFile)
		if err != nil {
			return err
		}
		if err := service.drainServices(ctx, tx.Candidate, names...); err != nil {
			return err
		}
		if err := service.command(ctx, infrastructureConfig(tx.Candidate), "down", "--remove-orphans"); err != nil {
			return err
		}
	} else if tx.Source.Layout == LayoutBlueGreen {
		names, err := applicationServices(tx.Candidate.ComposeFile)
		if err != nil {
			return err
		}
		if err := service.drainServices(ctx, tx.Candidate, names...); err != nil {
			return err
		}
	}
	if service.Recoveries == nil {
		return errors.New("recovery store unavailable")
	}
	if !tx.LayoutStarted {
		if err := service.drainServices(ctx, infrastructureConfig(tx.Source), "redis"); err != nil {
			return err
		}
	}
	if err := service.Recoveries.Restore(ctx, tx.Source, tx.RecoveryPointID, postgresDatabase{config: tx.Source, runner: service.runner()}); err != nil {
		return err
	}
	return service.Resume(ctx, tx.Source.ID)
}

// The legacy scheduler does not wait for schedule:run children. Freeze its spawner
// while existing children finish, then deliver TERM before allowing the parent to run.
func (service *Service) drainScheduler(ctx context.Context, config instance.Config) error {
	ids, err := service.containerIDs(ctx, config, "scheduler")
	if err != nil {
		return err
	}
	for _, id := range ids {
		var command limitedBuffer
		if err := service.runner().Run(ctx, nil, &command, "docker", "inspect", "--format={{json .Config.Cmd}}", id); err != nil {
			return err
		}
		if !strings.Contains(command.String(), `"schedule:work"`) {
			continue
		}
		var running limitedBuffer
		if err := service.runner().Run(ctx, nil, &running, "docker", "inspect", "--format={{.State.Running}}", id); err != nil {
			return err
		}
		if strings.TrimSpace(running.String()) != "true" {
			continue
		}
		if err := service.runner().Run(ctx, nil, io.Discard, "docker", "update", "--restart=no", id); err != nil {
			return err
		}
		if err := service.runner().Run(ctx, nil, io.Discard, "docker", "kill", "--signal=STOP", id); err != nil {
			return err
		}
		err := func() error {
			defer func() {
				recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				_ = service.runner().Run(recovery, nil, io.Discard, "docker", "kill", "--signal=CONT", id)
			}()
			for {
				var processes limitedBuffer
				if err := service.runner().Run(ctx, nil, &processes, "docker", "top", id, "-eo", "pid"); err != nil {
					return err
				}
				lines := strings.Fields(processes.String())
				if len(lines) < 2 {
					return errors.New("cannot establish legacy scheduler process state")
				}
				if len(lines) == 2 {
					break
				}
				if err := waitContext(ctx, time.Second); err != nil {
					return fmt.Errorf("legacy scheduler still has child tasks: %w", err)
				}
			}
			return service.runner().Run(ctx, nil, io.Discard, "docker", "kill", "--signal=TERM", id)
		}()
		if err != nil {
			return err
		}
	}
	return service.drainServices(ctx, config, "scheduler")
}

func (service *Service) runningServices(ctx context.Context, config instance.Config) error {
	expected, err := applicationServices(config.ComposeFile)
	if err != nil {
		return err
	}
	output, err := service.output(ctx, config, "ps", "--all", "--format", "json")
	if err != nil {
		return err
	}
	type row struct {
		Service string
		State   string
		Health  string
	}
	var rows []row
	if strings.HasPrefix(strings.TrimSpace(output), "[") {
		if err := json.Unmarshal([]byte(output), &rows); err != nil {
			return err
		}
	} else {
		decoder := json.NewDecoder(strings.NewReader(output))
		for {
			var item row
			err := decoder.Decode(&item)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			rows = append(rows, item)
		}
	}
	seen := map[string]bool{}
	for _, item := range rows {
		if item.Service == "init" {
			continue
		}
		if item.State != "running" || (item.Health != "" && item.Health != "healthy") {
			return fmt.Errorf("service %s is not ready", item.Service)
		}
		seen[item.Service] = true
	}
	for _, name := range expected {
		if !seen[name] {
			return fmt.Errorf("required service %s is absent", name)
		}
	}
	return nil
}
