package recoverycontrol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Checkpoint struct {
	SchemaVersion int    `json:"schema_version"`
	PointID       string `json:"point_id"`
	TransactionID string `json:"transaction_id"`
	AdminDigest   string `json:"admin_digest"`
}

// Checkpoint evidence is frozen while the source DB and topology still exist.
// It does not start a restore or rotate the current recovery epoch.
func (s Store) CaptureCheckpoint(instance, point, transaction, digest string) error {
	if !pointID.MatchString(point) || !identifier.MatchString(transaction) || !hex64.MatchString(digest) {
		return errors.New("invalid recovery checkpoint identity")
	}
	_, err := s.change(instance, true, func(a *Authority) error {
		if a.State.Phase != "ready" {
			return errors.New("checkpoint requires a ready source")
		}
		if previous, err := s.Checkpoint(instance, point, transaction); err == nil {
			if previous.AdminDigest != digest {
				return errors.New("frozen administrator digest changed")
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		dir := filepath.Join(s.StateDir, "recovery-control", instance, "checkpoints")
		if err := directory(dir, 0700); err != nil {
			return err
		}
		data, err := json.Marshal(Checkpoint{1, point, transaction, digest})
		if err != nil {
			return err
		}
		return atomic(filepath.Join(dir, transaction+".json"), data, 0600)
	})
	return err
}

func (s Store) Checkpoint(instance, point, transaction string) (Checkpoint, error) {
	var saved Checkpoint
	if instance != "primary" || !pointID.MatchString(point) || !identifier.MatchString(transaction) {
		return saved, errors.New("invalid recovery checkpoint identity")
	}
	path := filepath.Join(s.StateDir, "recovery-control", instance, "checkpoints", transaction+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return saved, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2048 || info.Mode().Perm()&0077 != 0 {
		return saved, errors.New("unsafe recovery checkpoint")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return saved, err
	}
	if json.Unmarshal(data, &saved) != nil || saved.SchemaVersion != 1 || saved.PointID != point || saved.TransactionID != transaction || !hex64.MatchString(saved.AdminDigest) {
		return saved, errors.New("recovery checkpoint identity mismatch")
	}
	return saved, nil
}
