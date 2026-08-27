package tufrepo

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

type InitializeOptions struct {
	KeysDir       string
	RepositoryDir string
	TargetsDir    string
	Now           func() time.Time
}

const (
	targetsValidity   = 90 * 24 * time.Hour
	snapshotValidity  = 30 * 24 * time.Hour
	timestampValidity = 7 * 24 * time.Hour
)

type trustedRepositoryState struct {
	root      *metadata.Metadata[metadata.RootType]
	timestamp *metadata.Metadata[metadata.TimestampType]
	snapshot  *metadata.Metadata[metadata.SnapshotType]
	targets   *metadata.Metadata[metadata.TargetsType]
}

func loadTrustedRepositoryState(metadataDir string) (trustedRepositoryState, error) {
	root, err := metadata.Root().FromFile(filepath.Join(metadataDir, "root.json"))
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("load root metadata: %w", err)
	}
	if err := root.VerifyDelegate("root", root); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify root metadata signatures: %w", err)
	}
	timestamp, err := metadata.Timestamp().FromFile(filepath.Join(metadataDir, "timestamp.json"))
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("load timestamp metadata: %w", err)
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify timestamp metadata signatures: %w", err)
	}
	snapshotMeta, ok := timestamp.Signed.Meta["snapshot.json"]
	if !ok || snapshotMeta == nil || snapshotMeta.Version < 1 {
		return trustedRepositoryState{}, errors.New("timestamp metadata does not reference a valid snapshot")
	}
	snapshotPath := filepath.Join(metadataDir, fmt.Sprintf("%d.snapshot.json", snapshotMeta.Version))
	snapshotBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("read snapshot metadata file: %w", err)
	}
	if err := snapshotMeta.VerifyLengthHashes(snapshotBytes); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify snapshot metadata file: %w", err)
	}
	snapshot, err := metadata.Snapshot().FromBytes(snapshotBytes)
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("load snapshot metadata: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify snapshot metadata signatures: %w", err)
	}
	targetsMeta, ok := snapshot.Signed.Meta["targets.json"]
	if !ok || targetsMeta == nil || targetsMeta.Version < 1 {
		return trustedRepositoryState{}, errors.New("snapshot metadata does not reference valid targets")
	}
	targetsPath := filepath.Join(metadataDir, fmt.Sprintf("%d.targets.json", targetsMeta.Version))
	targetsBytes, err := os.ReadFile(targetsPath)
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("read targets metadata file: %w", err)
	}
	if err := targetsMeta.VerifyLengthHashes(targetsBytes); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify targets metadata file: %w", err)
	}
	targets, err := metadata.Targets().FromBytes(targetsBytes)
	if err != nil {
		return trustedRepositoryState{}, fmt.Errorf("load targets metadata: %w", err)
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return trustedRepositoryState{}, fmt.Errorf("verify targets metadata signatures: %w", err)
	}

	return trustedRepositoryState{root: root, timestamp: timestamp, snapshot: snapshot, targets: targets}, nil
}

func Initialize(options InitializeOptions) error {
	if options.KeysDir == "" || options.RepositoryDir == "" || options.TargetsDir == "" {
		return errors.New("keys, repository, and target directories are required")
	}
	for _, path := range []string{options.KeysDir, options.RepositoryDir} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing path %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(options.KeysDir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	metadataDir := filepath.Join(options.RepositoryDir, "metadata")
	publishedTargetsDir := filepath.Join(options.RepositoryDir, "targets")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	if err := os.MkdirAll(publishedTargetsDir, 0o755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}

	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	root := metadata.Root(now.Add(730 * 24 * time.Hour))
	privateKeys := make(map[string][]ed25519.PrivateKey)
	for _, role := range []string{"root", "targets", "snapshot", "timestamp"} {
		count := 1
		if role == "root" {
			count = 3
		}
		for index := 1; index <= count; index++ {
			_, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return fmt.Errorf("generate %s key: %w", role, err)
			}
			privateKeys[role] = append(privateKeys[role], privateKey)
			name := role + ".pem"
			if count > 1 {
				name = fmt.Sprintf("%s-%d.pem", role, index)
			}
			if err := writePrivateKey(filepath.Join(options.KeysDir, name), privateKey); err != nil {
				return err
			}
			publicKey, err := metadata.KeyFromPublicKey(privateKey.Public())
			if err != nil {
				return fmt.Errorf("convert %s public key: %w", role, err)
			}
			if err := root.Signed.AddKey(publicKey, role); err != nil {
				return fmt.Errorf("add %s public key: %w", role, err)
			}
		}
	}
	root.Signed.Roles["root"].Threshold = 2
	for _, privateKey := range privateKeys["root"] {
		if err := signRoot(root, privateKey); err != nil {
			return err
		}
	}

	targets := metadata.Targets(now.Add(targetsValidity))
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
	if err := signTargets(targets, privateKeys["targets"][0]); err != nil {
		return err
	}
	targetsBytes, err := targets.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode targets metadata: %w", err)
	}

	snapshot := metadata.Snapshot(now.Add(snapshotValidity))
	snapshot.Signed.Meta["targets.json"] = metaFile(targets.Signed.Version, targetsBytes)
	if err := signSnapshot(snapshot, privateKeys["snapshot"][0]); err != nil {
		return err
	}
	snapshotBytes, err := snapshot.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode snapshot metadata: %w", err)
	}

	timestamp := metadata.Timestamp(now.Add(timestampValidity))
	timestamp.Signed.Meta["snapshot.json"] = metaFile(snapshot.Signed.Version, snapshotBytes)
	if err := signTimestamp(timestamp, privateKeys["timestamp"][0]); err != nil {
		return err
	}
	timestampBytes, err := timestamp.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode timestamp metadata: %w", err)
	}
	rootBytes, err := root.ToBytes(true)
	if err != nil {
		return fmt.Errorf("encode root metadata: %w", err)
	}

	metadataFiles := map[string][]byte{
		"root.json":       rootBytes,
		"1.root.json":     rootBytes,
		"1.targets.json":  targetsBytes,
		"1.snapshot.json": snapshotBytes,
		"timestamp.json":  timestampBytes,
	}
	for name, contents := range metadataFiles {
		if err := os.WriteFile(filepath.Join(metadataDir, name), contents, 0o644); err != nil {
			return fmt.Errorf("write metadata %s: %w", name, err)
		}
	}

	return nil
}

func listTargetFiles(root string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("target source contains a symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("target source contains a non-regular file: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	sort.Strings(paths)

	return paths, err
}

func nextMetadataVersion(metadataDir string, role string, current int64) (int64, error) {
	maximum := current
	entries, err := os.ReadDir(metadataDir)
	if err != nil {
		return 0, fmt.Errorf("inspect %s metadata versions: %w", role, err)
	}
	suffix := "." + role + ".json"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		versionText := strings.TrimSuffix(entry.Name(), suffix)
		version, parseErr := strconv.ParseInt(versionText, 10, 64)
		if parseErr != nil || version < 1 {
			continue
		}
		if version > maximum {
			maximum = version
		}
	}
	if maximum == int64(^uint64(0)>>1) {
		return 0, fmt.Errorf("%s metadata version is exhausted", role)
	}

	return maximum + 1, nil
}

func publishConsistentTarget(destinationRoot string, targetPath string, sourcePath string, info *metadata.TargetFiles) error {
	digest, ok := info.Hashes["sha256"]
	if !ok {
		return fmt.Errorf("target %s has no sha256 digest", targetPath)
	}
	targetDirectory := filepath.Dir(filepath.FromSlash(targetPath))
	targetName := hex.EncodeToString(digest) + "." + filepath.Base(filepath.FromSlash(targetPath))
	destination := filepath.Join(destinationRoot, targetDirectory, targetName)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, contents, 0o644); err != nil {
		return fmt.Errorf("publish target %s: %w", targetPath, err)
	}

	return nil
}

func metaFile(version int64, contents []byte) *metadata.MetaFiles {
	digest := sha256.Sum256(contents)

	return &metadata.MetaFiles{
		Version: version,
		Length:  int64(len(contents)),
		Hashes:  metadata.Hashes{"sha256": digest[:]},
	}
}

func writePrivateKey(path string, privateKey ed25519.PrivateKey) error {
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	if block == nil {
		return errors.New("encode private key PEM")
	}
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return fmt.Errorf("write private key %s: %w", filepath.Base(path), err)
	}

	return nil
}

func signer(privateKey ed25519.PrivateKey) (signature.Signer, error) {
	return signature.LoadSigner(privateKey, crypto.Hash(0))
}

func signRoot(root *metadata.Metadata[metadata.RootType], privateKey ed25519.PrivateKey) error {
	loaded, err := signer(privateKey)
	if err != nil {
		return err
	}
	_, err = root.Sign(loaded)

	return err
}

func signTargets(targets *metadata.Metadata[metadata.TargetsType], privateKey ed25519.PrivateKey) error {
	loaded, err := signer(privateKey)
	if err != nil {
		return err
	}
	_, err = targets.Sign(loaded)

	return err
}

func signSnapshot(snapshot *metadata.Metadata[metadata.SnapshotType], privateKey ed25519.PrivateKey) error {
	loaded, err := signer(privateKey)
	if err != nil {
		return err
	}
	_, err = snapshot.Sign(loaded)

	return err
}

func signTimestamp(timestamp *metadata.Metadata[metadata.TimestampType], privateKey ed25519.PrivateKey) error {
	loaded, err := signer(privateKey)
	if err != nil {
		return err
	}
	_, err = timestamp.Sign(loaded)

	return err
}
