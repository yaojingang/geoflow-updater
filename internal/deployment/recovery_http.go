package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

type recoveryContainer struct {
	ID     string `json:"Id"`
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	HostConfig struct{ RestartPolicy struct{ Name string } }
	State      struct {
		Running bool
		Status  string
		Health  *struct{ Status string }
	}
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
	}
}

// A stopped legacy container may have exited naturally and remain eligible for
// daemon restart. Keep every old ID disabled until replacement containers have
// durable guarded configuration, then enable only those verified new IDs.
func (service *Service) startRecoveredHTTP(ctx context.Context, config instance.Config) error {
	control := service.recoveryControl()
	authority, err := control.Read(config.ID)
	if err != nil {
		return err
	}
	if authority.State.Phase != "http_ready" || authority.State.TransactionID == nil || authority.PreparationSHA256 == "" || authority.RedisManifestSHA256 == "" {
		return errors.New("recovery HTTP evidence is incomplete")
	}
	oldIDs, err := service.containerIDs(ctx, config, "app", "web")
	if err != nil {
		return err
	}
	if err := service.drainContainersSignal(ctx, oldIDs, "QUIT"); err != nil {
		return err
	}
	arguments, err := service.runtimeArguments(config)
	if err != nil {
		return err
	}
	// This overlay stays on disk across interruption. Creating a container must
	// never grant a restart policy before its mounts and image are inspected.
	overlay := filepath.Join(filepath.Dir(config.ComposeFile), "recovery-http.yml")
	if err := replaceContents(overlay, []byte("services:\n  app:\n    restart: \"no\"\n  web:\n    restart: \"no\"\n"), 0600); err != nil {
		return err
	}
	arguments = append(arguments, "-f", overlay, "up", "--no-start", "--no-deps", "--force-recreate", "app", "web")
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", arguments...); err != nil {
		return err
	}
	legacy, err := legacyAdapterRequired(config)
	if err != nil {
		return err
	}
	var ids []string
	for _, role := range []string{"app", "web"} {
		selected, err := service.containerIDs(ctx, config, role)
		if err != nil {
			return err
		}
		if len(selected) != 1 || slices.Contains(oldIDs, selected[0]) || slices.Contains(ids, selected[0]) {
			return errors.New("recovery HTTP container was not replaced uniquely")
		}
		container, err := service.inspectRecoveryContainer(ctx, selected[0])
		if err != nil {
			return err
		}
		if err := service.validateRecoveryContainer(config, role, selected[0], container, legacy); err != nil {
			return err
		}
		ids = append(ids, selected[0])
	}
	current, err := control.Read(config.ID)
	if err != nil {
		return err
	}
	if current.State.Phase != "http_ready" || current.State.Epoch != authority.State.Epoch || current.State.TransactionID == nil || *current.State.TransactionID != *authority.State.TransactionID {
		return errors.New("recovery generation changed during HTTP preparation")
	}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append([]string{"update", "--restart=unless-stopped"}, ids...)...); err != nil {
		return err
	}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append([]string{"start"}, ids...)...); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	for {
		ready := true
		for _, id := range ids {
			container, err := service.inspectRecoveryContainer(readyCtx, id)
			if err != nil {
				return err
			}
			if container.State.Status == "exited" || container.State.Status == "dead" || (container.State.Health != nil && container.State.Health.Status == "unhealthy") {
				return errors.New("recovery HTTP container failed readiness")
			}
			if !container.State.Running || (container.State.Health != nil && container.State.Health.Status != "healthy") {
				ready = false
			}
		}
		if ready {
			return nil
		}
		if err := waitContext(readyCtx, time.Second); err != nil {
			return fmt.Errorf("recovery HTTP readiness: %w", err)
		}
	}
}

func (service *Service) inspectRecoveryContainer(ctx context.Context, id string) (recoveryContainer, error) {
	var container recoveryContainer
	var out boundedOutput
	if err := service.runner().Run(ctx, nil, &out, "docker", "inspect", "--format={{json .}}", id); err != nil {
		return container, err
	}
	if out.exceeded || json.Unmarshal(out.Bytes(), &container) != nil || container.ID != id {
		return container, errors.New("invalid recovery container inspection")
	}
	return container, nil
}

func (service *Service) validateRecoveryContainer(config instance.Config, role, id string, container recoveryContainer, legacy bool) error {
	if container.ID != id || container.Config.Labels["com.docker.compose.service"] != role || container.State.Running || container.State.Status != "created" || container.HostConfig.RestartPolicy.Name != "no" {
		return errors.New("recovery HTTP container must be newly created with restart disabled")
	}
	data, err := os.ReadFile(config.EnvironmentFile)
	if err != nil {
		return err
	}
	expected, err := environmentValue(data, "GEOFLOW_"+strings.ToUpper(role)+"_IMAGE")
	if err != nil {
		return err
	}
	prefix := "ghcr.io/yaojingang/geoflow-" + role + "@sha256:"
	if !strings.HasPrefix(expected, prefix) || !recoveryDigest.MatchString(strings.TrimPrefix(expected, prefix)) || container.Config.Image != expected {
		return errors.New("recovery HTTP image does not match the selected digest")
	}
	if role != "app" {
		return nil
	}
	for _, entry := range []string{"GEOFLOW_RECOVERY_CONTRACT=1", "GEOFLOW_UPDATER_INSTANCE_ID=" + config.ID, "AUTO_MIGRATE=false", "AUTO_INSTALL_ONCE=false", "AUTO_OPTIMIZE=false"} {
		if !slices.Contains(container.Config.Env, entry) {
			return errors.New("recovery PHP environment is incomplete")
		}
		key, _, _ := strings.Cut(entry, "=")
		for _, candidate := range container.Config.Env {
			if strings.HasPrefix(candidate, key+"=") && candidate != entry {
				return errors.New("recovery PHP environment conflicts")
			}
		}
	}
	mounts := map[string]string{"/run/geoflow-recovery-control": service.recoveryControl().PublicDir(config.ID)}
	if legacy {
		for _, entry := range container.Config.Env {
			key, value, _ := strings.Cut(entry, "=")
			if (key == "PHP_INI_SCAN_DIR" && value != "/usr/local/etc/php/conf.d") || (key == "PHPRC" && value != "/usr/local/etc/php") {
				return errors.New("legacy PHP configuration would bypass the recovery guard")
			}
		}
		dir := filepath.Join(service.recoveryControl().StateDir, "recovery-control", config.ID, "legacy-adapter")
		mounts[legacyAdapterContainerDir] = dir
		mounts["/usr/local/etc/php/conf.d/zz-geoflow-recovery.ini"] = filepath.Join(dir, "legacy-recovery.ini")
	}
	for destination, source := range mounts {
		found := false
		for _, mount := range container.Mounts {
			if mount.Destination != destination {
				continue
			}
			if found || mount.Type != "bind" || mount.Source != source || mount.RW {
				return errors.New("unsafe recovery PHP mount")
			}
			found = true
		}
		if !found {
			return errors.New("recovery PHP guard mount is absent")
		}
	}
	return nil
}
