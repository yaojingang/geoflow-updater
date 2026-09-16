package deployment

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
	"gopkg.in/yaml.v3"
)

func (service *Service) recoveryControl() recoverycontrol.Store {
	root := service.StateDir
	if root == "" {
		root = "/var/lib/geoflow-updater"
	}
	return recoverycontrol.Store{StateDir: root}
}

// Overlay restored and signed Compose documents without changing signed bytes.
// Bind the directory so atomic publication remains visible in running PHP.
func (service *Service) runtimeArguments(config instance.Config) ([]string, error) {
	args := composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile)
	control := service.recoveryControl()
	if _, err := control.Read(config.ID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, enabled := os.Lstat(filepath.Join(control.StateDir, "recovery-control", config.ID, "initialized")); errors.Is(enabled, os.ErrNotExist) {
				return args, nil
			}
		}
		return nil, err
	}
	authority, err := control.Initialize(config.ID)
	if err != nil {
		return nil, err
	}
	legacy, err := legacyAdapterRequired(config)
	if err != nil {
		return nil, err
	}
	volumes := []string{control.PublicDir(config.ID) + ":/run/geoflow-recovery-control:ro"}
	if legacy {
		dir, err := service.legacyAdapterFiles(config.ID)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, dir+":"+legacyAdapterContainerDir+":ro")
		// Ordinary operation on 3.1 remains unchanged. Once recovery starts,
		// every newly created PHP role must enforce the fixed recovery guard.
		if authority.State.Phase != "ready" {
			volumes = append(volumes, filepath.Join(dir, "legacy-recovery.ini")+":/usr/local/etc/php/conf.d/zz-geoflow-recovery.ini:ro")
		}
	}
	data, err := os.ReadFile(config.ComposeFile)
	if err != nil {
		return nil, err
	}
	var document struct {
		Services map[string]yaml.Node `yaml:"services"`
	}
	if err = yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	services := map[string]any{}
	for name := range document.Services {
		if !composeServicePattern.MatchString(name) {
			return nil, errors.New("invalid recovery runtime service")
		}
		switch name {
		case "postgres", "redis", "web", "edge":
			continue
		}
		services[name] = map[string]any{
			"environment": map[string]string{"GEOFLOW_RECOVERY_CONTRACT": "1", "GEOFLOW_UPDATER_INSTANCE_ID": config.ID, "AUTO_MIGRATE": "false", "AUTO_INSTALL_ONCE": "false", "AUTO_OPTIMIZE": "false"},
			"volumes":     volumes,
		}
	}
	if len(services) == 0 {
		return args, nil
	}
	overlay, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(config.ComposeFile), "recovery-runtime.yml")
	if err = replaceContents(path, overlay, 0600); err != nil {
		return nil, err
	}
	return append(args, "-f", path), nil
}
