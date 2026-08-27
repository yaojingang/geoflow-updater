package tufrepo

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type PublishOptions struct {
	RepositoryDir    string
	TargetsDir       string
	TargetsKeyPath   string
	SnapshotKeyPath  string
	TimestampKeyPath string
	Now              func() time.Time
}

func Publish(options PublishOptions) error {
	if options.RepositoryDir == "" || options.TargetsDir == "" || options.TargetsKeyPath == "" || options.SnapshotKeyPath == "" || options.TimestampKeyPath == "" {
		return errors.New("repository, targets, and online role key paths are required")
	}
	metadataDir := filepath.Join(options.RepositoryDir, "metadata")
	publishedTargetsDir := filepath.Join(options.RepositoryDir, "targets")
	state, err := loadTrustedRepositoryState(metadataDir)
	if err != nil {
		return err
	}
	root := state.root
	oldTimestamp := state.timestamp
	oldSnapshot := state.snapshot
	oldTargets := state.targets
	oldTargetsVersion := oldTargets.Signed.Version
	if err := validateReleaseProgression(oldTargets, publishedTargetsDir, options.TargetsDir); err != nil {
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
	targets := metadata.Targets(now.Add(targetsValidity))
	targets.Signed.Version = oldTargetsVersion + 1
	targetPaths, err := listTargetFiles(options.TargetsDir)
	if err != nil {
		return err
	}
	for _, targetPath := range targetPaths {
		localPath := filepath.Join(options.TargetsDir, filepath.FromSlash(targetPath))
		info, err := metadata.TargetFile().FromFile(localPath, "sha256")
		if err != nil {
			return fmt.Errorf("hash target %s: %w", targetPath, err)
		}
		targets.Signed.Targets[targetPath] = info
		if err := publishConsistentTarget(publishedTargetsDir, targetPath, localPath, info); err != nil {
			return err
		}
	}
	if err := signTargets(targets, targetsKey); err != nil {
		return fmt.Errorf("sign targets metadata: %w", err)
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return fmt.Errorf("verify targets role against root: %w", err)
	}
	targetsBytes, err := targets.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode targets metadata: %w", err)
	}

	snapshot := metadata.Snapshot(now.Add(snapshotValidity))
	snapshot.Signed.Version = oldSnapshot.Signed.Version + 1
	snapshot.Signed.Meta["targets.json"] = metaFile(targets.Signed.Version, targetsBytes)
	if err := signSnapshot(snapshot, snapshotKey); err != nil {
		return fmt.Errorf("sign snapshot metadata: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return fmt.Errorf("verify snapshot role against root: %w", err)
	}
	snapshotBytes, err := snapshot.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode snapshot metadata: %w", err)
	}

	timestamp := metadata.Timestamp(now.Add(timestampValidity))
	timestamp.Signed.Version = oldTimestamp.Signed.Version + 1
	timestamp.Signed.Meta["snapshot.json"] = metaFile(snapshot.Signed.Version, snapshotBytes)
	if err := signTimestamp(timestamp, timestampKey); err != nil {
		return fmt.Errorf("sign timestamp metadata: %w", err)
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return fmt.Errorf("verify timestamp role against root: %w", err)
	}
	timestampBytes, err := timestamp.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode timestamp metadata: %w", err)
	}

	if err := writeImmutable(filepath.Join(metadataDir, fmt.Sprintf("%d.targets.json", targets.Signed.Version)), targetsBytes); err != nil {
		return err
	}
	if err := writeImmutable(filepath.Join(metadataDir, fmt.Sprintf("%d.snapshot.json", snapshot.Signed.Version)), snapshotBytes); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(metadataDir, "timestamp.json"), timestampBytes); err != nil {
		return err
	}

	return nil
}

type publicationManifest struct {
	SchemaVersion   int               `json:"schema_version"`
	ReleaseSequence uint64            `json:"release_sequence"`
	Version         string            `json:"version"`
	AppImage        string            `json:"app_image"`
	WebImage        string            `json:"web_image"`
	PostgresImages  map[string]string `json:"postgres_images"`
	RedisImages     map[string]string `json:"redis_images"`
	ComposeTarget   string            `json:"compose_target"`
}

func validateReleaseProgression(oldTargets *metadata.Metadata[metadata.TargetsType], publishedTargetsDir string, sourceTargetsDir string) error {
	newManifestPath := filepath.Join(sourceTargetsDir, "releases", "current.json")
	newContents, err := os.ReadFile(newManifestPath)
	if err != nil {
		return fmt.Errorf("read release manifest target: %w", err)
	}
	newManifest, err := decodePublicationManifest(newContents)
	if err != nil {
		return fmt.Errorf("validate release manifest target: %w", err)
	}
	composeContents, err := os.ReadFile(filepath.Join(sourceTargetsDir, filepath.FromSlash(newManifest.ComposeTarget)))
	if err != nil {
		return fmt.Errorf("read managed Compose target: %w", err)
	}
	if err := (managed.Release{
		Sequence:        newManifest.ReleaseSequence,
		Version:         newManifest.Version,
		AppImage:        newManifest.AppImage,
		WebImage:        newManifest.WebImage,
		PostgresImages:  newManifest.PostgresImages,
		RedisImages:     newManifest.RedisImages,
		ComposeTemplate: composeContents,
	}).Validate(); err != nil {
		return fmt.Errorf("validate managed release target: %w", err)
	}

	oldInfo, exists := oldTargets.Signed.Targets["releases/current.json"]
	if !exists {
		return nil
	}
	digest, ok := oldInfo.Hashes["sha256"]
	if !ok {
		return errors.New("published release manifest has no sha256 digest")
	}
	oldPath := filepath.Join(publishedTargetsDir, "releases", hex.EncodeToString(digest)+".current.json")
	oldContents, err := os.ReadFile(oldPath)
	if err != nil {
		return fmt.Errorf("read published release manifest: %w", err)
	}
	if err := oldInfo.VerifyLengthHashes(oldContents); err != nil {
		return fmt.Errorf("verify published release manifest: %w", err)
	}
	oldManifest, err := decodePublicationManifest(oldContents)
	if err != nil {
		return fmt.Errorf("decode published release manifest: %w", err)
	}
	if newManifest.ReleaseSequence <= oldManifest.ReleaseSequence {
		return fmt.Errorf("release sequence %d must be greater than published sequence %d", newManifest.ReleaseSequence, oldManifest.ReleaseSequence)
	}

	return nil
}

func decodePublicationManifest(contents []byte) (publicationManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest publicationManifest
	if err := decoder.Decode(&manifest); err != nil {
		return publicationManifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return publicationManifest{}, errors.New("release manifest contains trailing JSON")
		}
		return publicationManifest{}, err
	}
	if manifest.SchemaVersion != 1 || manifest.ComposeTarget != "deploy/docker-compose.managed.yml" {
		return publicationManifest{}, errors.New("release manifest schema or Compose target is invalid")
	}

	return manifest, nil
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key must be a regular file with mode 0600 or stricter")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, trailing := pem.Decode(contents)
	if block == nil || len(trailing) != 0 || block.Type != "PRIVATE KEY" {
		return nil, errors.New("invalid private key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("online role key must be Ed25519")
	}

	return privateKey, nil
}

func writeImmutable(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create immutable metadata %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	return nil
}

func writeAtomic(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".timestamp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish timestamp metadata: %w", err)
	}

	return nil
}
