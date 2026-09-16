package recoverycontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CheckProtocol is independent of active-operation state and release versions.
func CheckProtocol(stateDir string, protocol uint64) error {
	path := filepath.Join(stateDir, "minimum-updater-protocol")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 24 {
		return errors.New("invalid host updater protocol floor")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	minimum, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || minimum < MinimumProtocol {
		return errors.New("invalid host updater protocol floor")
	}
	if protocol < minimum {
		return fmt.Errorf("host recovery state requires updater protocol %d; downgrade refused", minimum)
	}
	return nil
}
