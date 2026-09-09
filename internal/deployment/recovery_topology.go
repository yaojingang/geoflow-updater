package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/update"
)

// Application containers retain external network IDs while stopped. Remove the
// drained slots before deleting infrastructure networks so later starts recreate
// their endpoints. Persistent volumes and managed files remain available.
func (service *Service) downInfrastructure(ctx context.Context, config instance.Config) error {
	if config.Layout == LayoutBlueGreen {
		if err := service.validateRecoveryTopology(config, config); err != nil {
			return err
		}
		slots, err := service.recoverySlots(config, []instance.Config{config})
		if err != nil {
			return err
		}
		for _, slot := range slots {
			if err := service.command(ctx, slot, "down", "--remove-orphans"); err != nil {
				return err
			}
		}
	}
	return service.command(ctx, infrastructureConfig(config), "down", "--remove-orphans")
}

// instance.yml can still name the source after the candidate infrastructure has
// started. Keep both recorded deployments in authorized recovery until it ends.
func (service *Service) recoveryTopologies(config instance.Config) ([]instance.Config, error) {
	configs := []instance.Config{config}
	tx, err := service.readTransaction(config.ID)
	if errors.Is(err, os.ErrNotExist) {
		return service.recoverySlots(config, configs)
	}
	if err != nil {
		return nil, err
	}
	switch tx.Status {
	case update.StatusSucceeded, update.StatusRolledBack, update.StatusFailed:
		return service.recoverySlots(config, configs)
	case "running", update.StatusRecoveryRequired:
	default:
		return nil, errors.New("unknown deployment transaction status during recovery")
	}
	if !tx.LayoutStarted {
		return service.recoverySlots(config, configs)
	}
	if tx.Candidate.Layout != LayoutBlueGreen {
		return nil, errors.New("pending layout has no blue-green candidate")
	}
	seen := map[string]bool{config.ComposeFile: true}
	for _, recorded := range []instance.Config{tx.Source, tx.Candidate} {
		if err := service.validateRecoveryTopology(config, recorded); err != nil {
			return nil, err
		}
		if !seen[recorded.ComposeFile] {
			configs = append(configs, recorded)
			seen[recorded.ComposeFile] = true
		}
	}
	return service.recoverySlots(config, configs)
}

func (service *Service) recoverySlots(current instance.Config, configs []instance.Config) ([]instance.Config, error) {
	seen := map[string]bool{}
	for _, config := range configs {
		seen[config.ComposeFile] = true
	}
	for _, config := range append([]instance.Config(nil), configs...) {
		if config.Layout != LayoutBlueGreen {
			continue
		}
		other := config
		other.ActiveSlot = otherSlot(config.ActiveSlot)
		other.ComposeFile = filepath.Join(service.instanceDirectory(config.ID), "slots", other.ActiveSlot, "docker-compose.yml")
		other.EnvironmentFile = filepath.Join(filepath.Dir(other.ComposeFile), "release.env")
		if seen[other.ComposeFile] {
			continue
		}
		if err := regularFile(other.ComposeFile); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err := service.validateRecoveryTopology(current, other); err != nil {
			return nil, err
		}
		configs = append(configs, other)
		seen[other.ComposeFile] = true
	}
	return configs, nil
}

func (service *Service) validateRecoveryTopology(current, recorded instance.Config) error {
	if recorded.SchemaVersion != 1 || recorded.ID != current.ID || recorded.Root != current.Root || recorded.ControlToken != current.ControlToken || recorded.ReleaseSequence == 0 || recorded.Version == "" {
		return errors.New("recovery topology does not match the enrolled instance")
	}
	directory := service.instanceDirectory(current.ID)
	paths := []string{recorded.ComposeFile, recorded.EnvironmentFile, recorded.ControlToken}
	switch recorded.Layout {
	case "":
	case LayoutBlueGreen:
		if !validSlot(recorded.ActiveSlot) || recorded.ComposeFile != filepath.Join(directory, "slots", recorded.ActiveSlot, "docker-compose.yml") || recorded.EnvironmentFile != filepath.Join(directory, "slots", recorded.ActiveSlot, "release.env") || recorded.InfraComposeFile != filepath.Join(directory, "infra", "docker-compose.yml") || recorded.InfraEnvironmentFile != filepath.Join(directory, "infra", "release.env") {
			return errors.New("recovery topology has unexpected slot or infrastructure paths")
		}
		paths = append(paths, recorded.InfraComposeFile, recorded.InfraEnvironmentFile)
	default:
		return errors.New("recovery topology has an unsupported layout")
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !inside(directory, path) {
			return errors.New("recovery topology escaped managed state")
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != filepath.Join(resolvedDirectory, relative) {
			return errors.New("recovery topology has an unavailable or symbolic path")
		}
		if err := regularFile(path); err != nil {
			return err
		}
	}
	return nil
}
