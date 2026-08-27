package recovery

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

const maxRestoreBytes int64 = 100 * 1024 * 1024 * 1024

var (
	instanceIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	recoveryPointPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}$`)
)

type Database interface {
	Dump(context.Context, io.Writer) error
	Restore(context.Context, io.Reader) error
}

type FileRecord struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
}

type Point struct {
	SchemaVersion   int                   `json:"schema_version"`
	ID              string                `json:"id"`
	InstanceID      string                `json:"instance_id"`
	Reason          string                `json:"reason"`
	CreatedAt       time.Time             `json:"created_at"`
	Version         string                `json:"version"`
	ReleaseSequence uint64                `json:"release_sequence"`
	Root            string                `json:"root"`
	Files           map[string]FileRecord `json:"files"`
}

type Store struct {
	BackupRoot string
	Keep       int
	Now        func() time.Time
	Random     io.Reader
}

func (store Store) Create(ctx context.Context, config instance.Config, reason string, database Database) (Point, error) {
	if database == nil {
		return Point{}, errors.New("database backup service is required")
	}
	backupRoot, err := store.backupRoot()
	if err != nil {
		return Point{}, err
	}
	if !instanceIDPattern.MatchString(config.ID) || config.Root == "" || config.ComposeFile == "" || config.EnvironmentFile == "" {
		return Point{}, errors.New("instance configuration is incomplete")
	}
	instanceRoot := filepath.Join(backupRoot, config.ID)
	if err := ensureDirectory(instanceRoot, 0o700); err != nil {
		return Point{}, fmt.Errorf("create recovery point directory: %w", err)
	}
	id, err := store.newID()
	if err != nil {
		return Point{}, err
	}
	finalPath := filepath.Join(instanceRoot, id)
	temporaryPath := filepath.Join(instanceRoot, "."+id+".partial")
	if err := os.Mkdir(temporaryPath, 0o700); err != nil {
		return Point{}, fmt.Errorf("create recovery point staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporaryPath)
		}
	}()

	files := make(map[string]FileRecord)
	databasePath := filepath.Join(temporaryPath, "database.dump")
	databaseFile, err := os.OpenFile(databasePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Point{}, fmt.Errorf("create database backup: %w", err)
	}
	if err := database.Dump(ctx, databaseFile); err != nil {
		_ = databaseFile.Close()
		return Point{}, fmt.Errorf("dump PostgreSQL database: %w", err)
	}
	if err := databaseFile.Sync(); err != nil {
		_ = databaseFile.Close()
		return Point{}, fmt.Errorf("sync database backup: %w", err)
	}
	if err := databaseFile.Close(); err != nil {
		return Point{}, fmt.Errorf("close database backup: %w", err)
	}
	if files["database.dump"], err = describeFile(databasePath); err != nil {
		return Point{}, err
	}

	storageArchive := filepath.Join(temporaryPath, "storage.tar.gz")
	if err := archiveStorage(filepath.Join(config.Root, "storage"), storageArchive); err != nil {
		return Point{}, err
	}
	if files["storage.tar.gz"], err = describeFile(storageArchive); err != nil {
		return Point{}, err
	}

	artifacts := map[string]string{
		"site.env":                   filepath.Join(config.Root, ".env.prod"),
		"version.json":               filepath.Join(config.Root, "version.json"),
		"managed/instance.yml":       filepath.Join(filepath.Dir(config.ComposeFile), "instance.yml"),
		"managed/release.env":        config.EnvironmentFile,
		"managed/docker-compose.yml": config.ComposeFile,
	}
	keys := make([]string, 0, len(artifacts))
	for name := range artifacts {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		destination := filepath.Join(temporaryPath, filepath.FromSlash(name))
		if err := copyRegularFile(artifacts[name], destination); err != nil {
			return Point{}, fmt.Errorf("back up %s: %w", name, err)
		}
		if files[name], err = describeFile(destination); err != nil {
			return Point{}, err
		}
	}

	point := Point{
		SchemaVersion:   1,
		ID:              id,
		InstanceID:      config.ID,
		Reason:          strings.TrimSpace(reason),
		CreatedAt:       store.now().UTC(),
		Version:         config.Version,
		ReleaseSequence: config.ReleaseSequence,
		Root:            config.Root,
		Files:           files,
	}
	manifest, err := json.MarshalIndent(point, "", "  ")
	if err != nil {
		return Point{}, fmt.Errorf("encode recovery point manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := writeExclusive(filepath.Join(temporaryPath, "manifest.json"), manifest, 0o600); err != nil {
		return Point{}, err
	}
	if err := syncDirectory(temporaryPath); err != nil {
		return Point{}, fmt.Errorf("sync recovery point: %w", err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Point{}, fmt.Errorf("commit recovery point: %w", err)
	}
	if err := syncDirectory(instanceRoot); err != nil {
		return Point{}, fmt.Errorf("sync committed recovery point: %w", err)
	}
	committed = true
	if err := store.prune(config.ID, id); err != nil {
		return point, fmt.Errorf("prune old recovery points: %w", err)
	}

	return point, nil
}

func (store Store) Restore(ctx context.Context, config instance.Config, id string, database Database) error {
	if database == nil {
		return errors.New("database restore service is required")
	}
	pointPath, point, err := store.load(config.ID, id)
	if err != nil {
		return err
	}
	if point.Root != config.Root {
		return errors.New("recovery point belongs to a different instance root")
	}
	expectedFiles := []string{
		"database.dump",
		"managed/docker-compose.yml",
		"managed/instance.yml",
		"managed/release.env",
		"site.env",
		"storage.tar.gz",
		"version.json",
	}
	if len(point.Files) != len(expectedFiles) {
		return errors.New("recovery point file set is incomplete")
	}
	for _, name := range expectedFiles {
		record, ok := point.Files[name]
		if !ok {
			return fmt.Errorf("recovery point is missing %s", name)
		}
		if err := verifyFile(filepath.Join(pointPath, filepath.FromSlash(name)), record); err != nil {
			return fmt.Errorf("verify recovery point %s: %w", name, err)
		}
	}

	destinations := map[string]string{
		"site.env":                   filepath.Join(config.Root, ".env.prod"),
		"version.json":               filepath.Join(config.Root, "version.json"),
		"managed/instance.yml":       filepath.Join(filepath.Dir(config.ComposeFile), "instance.yml"),
		"managed/release.env":        config.EnvironmentFile,
		"managed/docker-compose.yml": config.ComposeFile,
	}
	for _, name := range expectedFiles {
		destination, ok := destinations[name]
		if !ok {
			continue
		}
		if err := replaceFile(filepath.Join(pointPath, filepath.FromSlash(name)), destination, point.Files[name]); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}

	databaseFile, err := os.Open(filepath.Join(pointPath, "database.dump"))
	if err != nil {
		return fmt.Errorf("open database backup: %w", err)
	}
	if err := database.Restore(ctx, databaseFile); err != nil {
		_ = databaseFile.Close()
		return fmt.Errorf("restore PostgreSQL database: %w", err)
	}
	if err := databaseFile.Close(); err != nil {
		return fmt.Errorf("close database backup: %w", err)
	}

	if err := restoreStorage(config.Root, filepath.Join(pointPath, "storage.tar.gz"), id); err != nil {
		return err
	}

	return nil
}

func (store Store) List(instanceID string) ([]Point, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return nil, errors.New("managed instance identifier is invalid")
	}
	backupRoot, err := store.backupRoot()
	if err != nil {
		return nil, err
	}
	instanceRoot := filepath.Join(backupRoot, instanceID)
	entries, err := os.ReadDir(instanceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return []Point{}, nil
	}
	if err != nil {
		return nil, err
	}
	points := make([]Point, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !recoveryPointPattern.MatchString(entry.Name()) {
			continue
		}
		_, point, loadErr := store.load(instanceID, entry.Name())
		if loadErr != nil {
			continue
		}
		points = append(points, point)
	}
	sort.Slice(points, func(left, right int) bool { return points[left].CreatedAt.After(points[right].CreatedAt) })

	return points, nil
}

func (store Store) prune(instanceID string, protectedID string) error {
	keep := store.Keep
	if keep == 0 {
		keep = 5
	}
	if keep < 1 {
		return errors.New("recovery point retention must be positive")
	}
	points, err := store.List(instanceID)
	if err != nil {
		return err
	}
	retainedIDs := map[string]struct{}{protectedID: {}}
	for _, point := range points {
		if len(retainedIDs) >= keep {
			break
		}
		retainedIDs[point.ID] = struct{}{}
	}
	for _, point := range points {
		if _, retained := retainedIDs[point.ID]; retained {
			continue
		}
		backupRoot, rootErr := store.backupRoot()
		if rootErr != nil {
			return rootErr
		}
		path := filepath.Join(backupRoot, instanceID, point.ID)
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recovery point retention target is unsafe")
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func (store Store) load(instanceID string, id string) (string, Point, error) {
	if !instanceIDPattern.MatchString(instanceID) || !recoveryPointPattern.MatchString(id) {
		return "", Point{}, errors.New("recovery point identifier is invalid")
	}
	backupRoot, err := store.backupRoot()
	if err != nil {
		return "", Point{}, err
	}
	pointPath := filepath.Join(backupRoot, instanceID, id)
	manifestPath := filepath.Join(pointPath, "manifest.json")
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return "", Point{}, errors.New("recovery point manifest is unavailable")
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", Point{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var point Point
	if err := decoder.Decode(&point); err != nil {
		return "", Point{}, fmt.Errorf("decode recovery point manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", Point{}, errors.New("recovery point manifest contains trailing JSON")
	}
	if point.SchemaVersion != 1 || point.ID != id || point.InstanceID != instanceID || point.ReleaseSequence == 0 || point.CreatedAt.IsZero() {
		return "", Point{}, errors.New("recovery point manifest is invalid")
	}

	return pointPath, point, nil
}

func archiveStorage(storageRoot string, destination string) error {
	info, err := os.Lstat(storageRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("instance storage directory is unavailable or unsafe")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	if err != nil {
		_ = file.Close()
		return err
	}
	tarWriter := tar.NewWriter(gzipWriter)
	var total int64
	walkErr := filepath.WalkDir(storageRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("storage contains unsupported entry %s", path)
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || total > maxRestoreBytes-info.Size() {
				return errors.New("instance storage exceeds the recovery point limit")
			}
			total += info.Size()
		}
		relative, err := filepath.Rel(storageRoot, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		uid, gid, err := owner(info)
		if err != nil {
			return err
		}
		header.Uid = uid
		header.Gid = gid
		header.Name = "storage"
		if relative != "." {
			header.Name = filepath.ToSlash(filepath.Join("storage", relative))
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, source)
		closeErr := source.Close()
		return errors.Join(copyErr, closeErr)
	})
	closeErr := errors.Join(tarWriter.Close(), gzipWriter.Close(), file.Sync(), file.Close())
	if walkErr != nil || closeErr != nil {
		return errors.Join(walkErr, closeErr)
	}

	return nil
}

func restoreStorage(root string, archivePath string, id string) error {
	stageRoot, err := os.MkdirTemp(root, ".geoflow-updater-restore-")
	if err != nil {
		return fmt.Errorf("create storage restore staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	if err := extractStorage(archivePath, stageRoot); err != nil {
		return err
	}
	stagedStorage := filepath.Join(stageRoot, "storage")
	if info, err := os.Lstat(stagedStorage); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("recovery point storage archive is incomplete")
	}
	currentStorage := filepath.Join(root, "storage")
	oldStorage := filepath.Join(root, ".geoflow-updater-storage-old-"+id)
	if _, err := os.Lstat(oldStorage); err == nil {
		return errors.New("previous storage restore staging directory still exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(currentStorage, oldStorage); err != nil {
		return fmt.Errorf("stage current storage for restore: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.Rename(oldStorage, currentStorage)
		}
	}()
	if err := os.Rename(stagedStorage, currentStorage); err != nil {
		return fmt.Errorf("activate restored storage: %w", err)
	}
	restored = true
	syncErr := syncDirectory(root)
	removeErr := os.RemoveAll(oldStorage)
	finalSyncErr := syncDirectory(root)
	if err := errors.Join(syncErr, removeErr, finalSyncErr); err != nil {
		return fmt.Errorf("commit restored storage: %w", err)
	}

	return nil
}

func extractStorage(archivePath string, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if !filepath.IsLocal(name) || (name != "storage" && !strings.HasPrefix(name, "storage"+string(filepath.Separator))) {
			return errors.New("storage archive contains an unsafe path")
		}
		total += header.Size
		if header.Size < 0 || total > maxRestoreBytes {
			return errors.New("storage archive exceeds the restore limit")
		}
		path := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
			if err := os.Chmod(path, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
			if err := os.Chown(path, header.Uid, header.Gid); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o666)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
			if err := os.Chown(path, header.Uid, header.Gid); err != nil {
				return err
			}
		default:
			return errors.New("storage archive contains an unsupported entry")
		}
	}
}

func copyRegularFile(source string, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := writeExclusive(destination, contents, info.Mode().Perm()&0o660); err != nil {
		return err
	}
	uid, gid, err := owner(info)
	if err != nil {
		return err
	}
	return os.Chown(destination, uid, gid)
}

func replaceFile(source string, destination string, record FileRecord) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".geoflow-updater-restore-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(os.FileMode(record.Mode) & 0o660); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chown(temporaryPath, record.UID, record.GID); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func describeFile(path string) (FileRecord, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return FileRecord{}, errors.New("backup artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return FileRecord{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return FileRecord{}, errors.Join(copyErr, closeErr)
	}

	uid, gid, err := owner(info)
	if err != nil {
		return FileRecord{}, err
	}

	return FileRecord{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size(), Mode: uint32(info.Mode().Perm()), UID: uid, GID: gid}, nil
}

func verifyFile(path string, expected FileRecord) error {
	actual, err := describeFile(path)
	if err != nil {
		return err
	}
	if actual.SHA256 != expected.SHA256 || actual.Size != expected.Size || actual.Mode != expected.Mode || actual.UID != expected.UID || actual.GID != expected.GID {
		return errors.New("backup artifact checksum, size, mode, or ownership does not match its manifest")
	}

	return nil
}

func owner(info os.FileInfo) (int, int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("file ownership metadata is unavailable")
	}
	return int(stat.Uid), int(stat.Gid), nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("recovery point directory is unsafe")
	}
	return nil
}

func writeExclusive(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func (store Store) backupRoot() (string, error) {
	root := store.BackupRoot
	if root == "" {
		root = "/var/backups/geoflow-updater"
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("backup root must be absolute")
	}
	return filepath.Clean(root), nil
}

func (store Store) newID() (string, error) {
	random := store.Random
	if random == nil {
		random = rand.Reader
	}
	suffix := make([]byte, 4)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return "", fmt.Errorf("generate recovery point id: %w", err)
	}
	return store.now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix), nil
}

func (store Store) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}
