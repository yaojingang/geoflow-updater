package tufrepo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
)

type RefreshOptions struct {
	RepositoryDir    string
	TargetsKeyPath   string
	SnapshotKeyPath  string
	TimestampKeyPath string
	Now              func() time.Time
}

type RefreshOnlineOptions struct {
	RepositoryDir    string
	SnapshotKeyPath  string
	TimestampKeyPath string
	Now              func() time.Time
}

// RefreshOnline extends snapshot and timestamp metadata without accessing the targets key.
func RefreshOnline(options RefreshOnlineOptions) error {
	if options.RepositoryDir == "" || options.SnapshotKeyPath == "" || options.TimestampKeyPath == "" {
		return errors.New("repository, snapshot key, and timestamp key paths are required")
	}
	metadataDir := filepath.Join(options.RepositoryDir, "metadata")
	state, err := loadTrustedRepositoryState(metadataDir)
	if err != nil {
		return err
	}
	snapshotVersion, err := nextMetadataVersion(metadataDir, "snapshot", state.snapshot.Signed.Version)
	if err != nil {
		return err
	}
	snapshotKey, err := LoadPrivateKey(options.SnapshotKeyPath)
	if err != nil {
		return fmt.Errorf("load snapshot key: %w", err)
	}
	timestampKey, err := LoadPrivateKey(options.TimestampKeyPath)
	if err != nil {
		return fmt.Errorf("load timestamp key: %w", err)
	}
	targetsBytes, err := os.ReadFile(filepath.Join(metadataDir, fmt.Sprintf("%d.targets.json", state.targets.Signed.Version)))
	if err != nil {
		return fmt.Errorf("read current targets metadata: %w", err)
	}

	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	snapshot := metadata.Snapshot(now.Add(snapshotValidity))
	snapshot.Signed.Version = snapshotVersion
	snapshot.Signed.Meta["targets.json"] = metaFile(state.targets.Signed.Version, targetsBytes)
	if err := signSnapshot(snapshot, snapshotKey); err != nil {
		return fmt.Errorf("sign refreshed snapshot metadata: %w", err)
	}
	if err := state.root.VerifyDelegate("snapshot", snapshot); err != nil {
		return fmt.Errorf("verify refreshed snapshot role against root: %w", err)
	}
	snapshotBytes, err := snapshot.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode refreshed snapshot metadata: %w", err)
	}

	timestamp := metadata.Timestamp(now.Add(timestampValidity))
	timestamp.Signed.Version = state.timestamp.Signed.Version + 1
	timestamp.Signed.Meta["snapshot.json"] = metaFile(snapshot.Signed.Version, snapshotBytes)
	if err := signTimestamp(timestamp, timestampKey); err != nil {
		return fmt.Errorf("sign refreshed timestamp metadata: %w", err)
	}
	if err := state.root.VerifyDelegate("timestamp", timestamp); err != nil {
		return fmt.Errorf("verify refreshed timestamp role against root: %w", err)
	}
	timestampBytes, err := timestamp.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode refreshed timestamp metadata: %w", err)
	}

	if err := writeImmutable(filepath.Join(metadataDir, fmt.Sprintf("%d.snapshot.json", snapshot.Signed.Version)), snapshotBytes); err != nil {
		return err
	}

	return writeAtomic(filepath.Join(metadataDir, "timestamp.json"), timestampBytes)
}

// Refresh re-signs unchanged targets and extends online metadata expiry.
func Refresh(options RefreshOptions) error {
	if options.RepositoryDir == "" || options.TargetsKeyPath == "" || options.SnapshotKeyPath == "" || options.TimestampKeyPath == "" {
		return errors.New("repository and online role key paths are required")
	}
	metadataDir := filepath.Join(options.RepositoryDir, "metadata")
	state, err := loadTrustedRepositoryState(metadataDir)
	if err != nil {
		return err
	}
	root := state.root
	oldTimestamp := state.timestamp
	oldSnapshot := state.snapshot
	targets := state.targets
	targetsVersion, err := nextMetadataVersion(metadataDir, "targets", targets.Signed.Version)
	if err != nil {
		return err
	}
	snapshotVersion, err := nextMetadataVersion(metadataDir, "snapshot", oldSnapshot.Signed.Version)
	if err != nil {
		return err
	}

	targetsKey, err := LoadPrivateKey(options.TargetsKeyPath)
	if err != nil {
		return fmt.Errorf("load targets key: %w", err)
	}
	snapshotKey, err := LoadPrivateKey(options.SnapshotKeyPath)
	if err != nil {
		return fmt.Errorf("load snapshot key: %w", err)
	}
	timestampKey, err := LoadPrivateKey(options.TimestampKeyPath)
	if err != nil {
		return fmt.Errorf("load timestamp key: %w", err)
	}

	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	targets.Signed.Version = targetsVersion
	targets.Signed.Expires = now.Add(targetsValidity)
	targets.ClearSignatures()
	if err := signTargets(targets, targetsKey); err != nil {
		return fmt.Errorf("sign refreshed targets metadata: %w", err)
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return fmt.Errorf("verify refreshed targets role against root: %w", err)
	}
	targetsBytes, err := targets.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode refreshed targets metadata: %w", err)
	}

	snapshot := metadata.Snapshot(now.Add(snapshotValidity))
	snapshot.Signed.Version = snapshotVersion
	snapshot.Signed.Meta["targets.json"] = metaFile(targets.Signed.Version, targetsBytes)
	if err := signSnapshot(snapshot, snapshotKey); err != nil {
		return fmt.Errorf("sign refreshed snapshot metadata: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return fmt.Errorf("verify refreshed snapshot role against root: %w", err)
	}
	snapshotBytes, err := snapshot.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode refreshed snapshot metadata: %w", err)
	}

	timestamp := metadata.Timestamp(now.Add(timestampValidity))
	timestamp.Signed.Version = oldTimestamp.Signed.Version + 1
	timestamp.Signed.Meta["snapshot.json"] = metaFile(snapshot.Signed.Version, snapshotBytes)
	if err := signTimestamp(timestamp, timestampKey); err != nil {
		return fmt.Errorf("sign refreshed timestamp metadata: %w", err)
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return fmt.Errorf("verify refreshed timestamp role against root: %w", err)
	}
	timestampBytes, err := timestamp.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode refreshed timestamp metadata: %w", err)
	}

	if err := writeImmutable(filepath.Join(metadataDir, fmt.Sprintf("%d.targets.json", targets.Signed.Version)), targetsBytes); err != nil {
		return err
	}
	if err := writeImmutable(filepath.Join(metadataDir, fmt.Sprintf("%d.snapshot.json", snapshot.Signed.Version)), snapshotBytes); err != nil {
		return err
	}

	return writeAtomic(filepath.Join(metadataDir, "timestamp.json"), timestampBytes)
}
