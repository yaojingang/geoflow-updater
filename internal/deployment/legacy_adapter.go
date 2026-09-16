package deployment

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

// Provenance: signed TUF targets version 5, releases/current.json SHA-256
// 49b5a7150ed89099beba0e047e097f56018016116dad4303c016c507110ba977.
const legacyRecoveryAppImage = "ghcr.io/yaojingang/geoflow-app@sha256:94c01bfd52941cf48ad9693fce30fc571c29353189fb5e128d0e0752daa36448"
const legacyRecoverySourceCommit = "6c963783bbf49f0b5ec9d0121a924ee54005b60d"

const legacyAdapterContainerDir = "/run/geoflow-recovery-adapter"

//go:embed legacy-recovery.php
var legacyRecoveryScript []byte

//go:embed legacy-http-guard.php
var legacyHTTPGuard []byte

func (service *Service) legacyAdapterFiles(id string) (string, error) {
	dir := filepath.Join(service.recoveryControl().StateDir, "recovery-control", id, "legacy-adapter")
	if err := safeDirectory(dir, 0755); err != nil {
		return "", err
	}
	// Docker binds this directory directly; PHP uid 33 must traverse it even
	// when the host daemon was started with umask 0027.
	if err := os.Chmod(dir, 0755); err != nil {
		return "", err
	}
	files := map[string][]byte{
		"legacy-recovery.php":   legacyRecoveryScript,
		"legacy-http-guard.php": legacyHTTPGuard,
		"legacy-recovery.ini":   []byte("auto_prepend_file=" + legacyAdapterContainerDir + "/legacy-http-guard.php\n"),
	}
	for name, content := range files {
		if err := replaceContents(filepath.Join(dir, name), content, 0444); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func legacyAdapterRequired(config instance.Config) (bool, error) {
	data, err := os.ReadFile(config.EnvironmentFile)
	if err != nil {
		return false, err
	}
	image, err := environmentValue(data, "GEOFLOW_APP_IMAGE")
	if err != nil {
		return false, err
	}
	if image != legacyRecoveryAppImage {
		return false, nil
	}
	version, err := environmentValue(data, "GEOFLOW_VERSION")
	if err != nil {
		return false, err
	}
	if config.Version != "3.1.0" || version != "3.1.0" {
		return false, errors.New("signed legacy recovery image version mismatch")
	}
	return true, nil
}
