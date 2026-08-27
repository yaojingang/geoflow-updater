package recovery_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
)

type database struct {
	dump     []byte
	restored []byte
}

func (db *database) Dump(_ context.Context, writer io.Writer) error {
	_, err := writer.Write(db.dump)
	return err
}

func (db *database) Restore(_ context.Context, reader io.Reader) error {
	contents, err := io.ReadAll(reader)
	db.restored = contents
	return err
}

func TestStoreCreatesVerifiedRecoveryPointAndRestoresDatabaseStorageAndManagedState(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("APP_ENV=production\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{\"version\":\"2.4.0\"}\n"), 0o644)
	mustWrite(t, filepath.Join(root, "storage", "app", "customer.txt"), []byte("customer data"), 0o640)
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("redis state"), 0o600)
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("old instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("old release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("old compose\n"), 0o640)

	config := instance.Config{
		ID:              "primary",
		Root:            root,
		ComposeFile:     filepath.Join(stateDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(stateDir, "release.env"),
		Version:         "2.4.0",
		ReleaseSequence: 17,
	}
	db := &database{dump: []byte("postgres custom dump")}
	store := recovery.Store{
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		Now:        func() time.Time { return time.Date(2026, time.August, 27, 12, 34, 56, 0, time.UTC) },
	}
	point, err := store.Create(context.Background(), config, "before update", db)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("changed env\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{\"version\":\"2.5.0\"}\n"), 0o644)
	mustWrite(t, filepath.Join(root, "storage", "app", "customer.txt"), []byte("changed data"), 0o640)
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("changed redis"), 0o600)
	mustWrite(t, config.EnvironmentFile, []byte("new release\n"), 0o640)

	if err := store.Restore(context.Background(), config, point.ID, db); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	assertContents(t, filepath.Join(root, ".env.prod"), "APP_ENV=production\n")
	assertContents(t, filepath.Join(root, "version.json"), "{\"version\":\"2.4.0\"}\n")
	assertContents(t, filepath.Join(root, "storage", "app", "customer.txt"), "customer data")
	assertContents(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), "redis state")
	assertContents(t, config.EnvironmentFile, "old release\n")
	if !bytes.Equal(db.restored, db.dump) {
		t.Fatalf("restored database = %q, want %q", db.restored, db.dump)
	}
}

func TestStoreRefusesATamperedRecoveryPointBeforeRestoring(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("original env\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{}\n"), 0o644)
	mustWrite(t, filepath.Join(root, "storage", "file.txt"), []byte("original"), 0o640)
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("redis"), 0o600)
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	backupRoot := filepath.Join(t.TempDir(), "backups")
	db := &database{dump: []byte("database")}
	store := recovery.Store{BackupRoot: backupRoot}
	point, err := store.Create(context.Background(), config, "manual", db)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	mustWrite(t, filepath.Join(backupRoot, "primary", point.ID, "database.dump"), []byte("tampered"), 0o600)
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("current env\n"), 0o600)

	if err := store.Restore(context.Background(), config, point.ID, db); err == nil {
		t.Fatal("Restore() accepted a tampered recovery point")
	}
	assertContents(t, filepath.Join(root, ".env.prod"), "current env\n")
}

func TestStoreRetainsTheNewestConfiguredRecoveryPoints(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("APP_ENV=production\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{}\n"), 0o640)
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docker-data", "prod", "redis"), 0o750); err != nil {
		t.Fatalf("mkdir redis: %v", err)
	}
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	clock := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := recovery.Store{
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		Keep:       2,
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
	}
	for index := 0; index < 3; index++ {
		if _, err := store.Create(context.Background(), config, "retention", &database{dump: []byte("database")}); err != nil {
			t.Fatalf("Create(%d) error = %v", index, err)
		}
	}
	points, err := store.List("primary")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(points) != 2 || !points[0].CreatedAt.After(points[1].CreatedAt) {
		t.Fatalf("retained points = %#v", points)
	}
}

func TestStoreRetainsNewestPreUpdateCheckpointAcrossManualBackups(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("APP_ENV=production\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{}\n"), 0o640)
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0o750); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docker-data", "prod", "redis"), 0o750); err != nil {
		t.Fatalf("mkdir redis: %v", err)
	}
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	clock := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	store := recovery.Store{
		BackupRoot: filepath.Join(t.TempDir(), "backups"),
		Keep:       2,
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
	}
	db := &database{dump: []byte("database")}
	updatePoint, err := store.Create(context.Background(), config, "update-to-2.5.0", db)
	if err != nil {
		t.Fatalf("Create(update checkpoint) error = %v", err)
	}
	if _, err := store.Create(context.Background(), config, "manual", db); err != nil {
		t.Fatalf("Create(first manual backup) error = %v", err)
	}
	latestManualPoint, err := store.Create(context.Background(), config, "manual", db)
	if err != nil {
		t.Fatalf("Create(second manual backup) error = %v", err)
	}

	points, err := store.List("primary")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("retained point count = %d, want 2", len(points))
	}
	retained := make(map[string]struct{}, len(points))
	for _, point := range points {
		retained[point.ID] = struct{}{}
	}
	if _, ok := retained[updatePoint.ID]; !ok {
		t.Fatalf("pre-update checkpoint %s was pruned", updatePoint.ID)
	}
	if _, ok := retained[latestManualPoint.ID]; !ok {
		t.Fatalf("latest manual recovery point %s was pruned", latestManualPoint.ID)
	}
}

func TestStoreRecoversAnInterruptedStorageSwapBeforeRetryingRestore(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("APP_ENV=production\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{\"version\":\"2.4.0\"}\n"), 0o640)
	mustWrite(t, filepath.Join(root, "storage", "app", "customer.txt"), []byte("backup data"), 0o640)
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("redis"), 0o600)
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	store := recovery.Store{BackupRoot: filepath.Join(t.TempDir(), "backups")}
	db := &database{dump: []byte("database")}
	point, err := store.Create(context.Background(), config, "before update", db)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	mustWrite(t, filepath.Join(root, "storage", "app", "customer.txt"), []byte("current data"), 0o640)
	oldStorage := filepath.Join(root, ".geoflow-updater-storage-old-"+point.ID)
	if err := os.Rename(filepath.Join(root, "storage"), oldStorage); err != nil {
		t.Fatalf("simulate interrupted storage swap: %v", err)
	}

	if err := store.Restore(context.Background(), config, point.ID, db); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	assertContents(t, filepath.Join(root, "storage", "app", "customer.txt"), "backup data")
	if _, err := os.Lstat(oldStorage); !os.IsNotExist(err) {
		t.Fatalf("old storage path still exists after recovery: %v", err)
	}
}

func TestStoreStagesEveryDirectoryArchiveBeforeMutatingManagedState(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("original env\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{}\n"), 0o640)
	mustWrite(t, filepath.Join(root, "storage", "file.txt"), []byte("storage"), 0o640)
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("redis"), 0o600)
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	backupRoot := filepath.Join(t.TempDir(), "backups")
	db := &database{dump: []byte("database")}
	store := recovery.Store{BackupRoot: backupRoot}
	point, err := store.Create(context.Background(), config, "manual", db)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	invalidArchive := []byte("valid checksum, invalid gzip")
	archivePath := filepath.Join(backupRoot, "primary", point.ID, "storage.tar.gz")
	mustWrite(t, archivePath, invalidArchive, 0o600)
	manifestPath := filepath.Join(backupRoot, "primary", point.ID, "manifest.json")
	manifestContents, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest recovery.Point
	if err := json.Unmarshal(manifestContents, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	digest := sha256.Sum256(invalidArchive)
	record := manifest.Files["storage.tar.gz"]
	record.SHA256 = hex.EncodeToString(digest[:])
	record.Size = int64(len(invalidArchive))
	manifest.Files["storage.tar.gz"] = record
	updatedManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	mustWrite(t, manifestPath, append(updatedManifest, '\n'), 0o600)
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("current env\n"), 0o600)
	db.restored = nil

	if err := store.Restore(context.Background(), config, point.ID, db); err == nil {
		t.Fatal("Restore() accepted an invalid storage archive")
	}
	assertContents(t, filepath.Join(root, ".env.prod"), "current env\n")
	if db.restored != nil {
		t.Fatalf("database was mutated before archive staging: %q", db.restored)
	}
}

func TestStoreRestoresRegularFileModeDespiteProcessUmask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "site")
	stateDir := filepath.Join(t.TempDir(), "state", "instances", "primary")
	mustWrite(t, filepath.Join(root, ".env.prod"), []byte("env\n"), 0o600)
	mustWrite(t, filepath.Join(root, "version.json"), []byte("{}\n"), 0o640)
	mustWrite(t, filepath.Join(root, "storage", "shared.txt"), []byte("shared"), 0o664)
	if err := os.Chmod(filepath.Join(root, "storage", "shared.txt"), 0o664); err != nil {
		t.Fatalf("chmod source file: %v", err)
	}
	mustWrite(t, filepath.Join(root, "docker-data", "prod", "redis", "appendonly.aof"), []byte("redis"), 0o600)
	mustWrite(t, filepath.Join(stateDir, "instance.yml"), []byte("instance\n"), 0o640)
	mustWrite(t, filepath.Join(stateDir, "release.env"), []byte("release\n"), 0o660)
	if err := os.Chmod(filepath.Join(stateDir, "release.env"), 0o660); err != nil {
		t.Fatalf("chmod release environment: %v", err)
	}
	mustWrite(t, filepath.Join(stateDir, "docker-compose.managed.yml"), []byte("compose\n"), 0o640)
	config := instance.Config{ID: "primary", Root: root, ComposeFile: filepath.Join(stateDir, "docker-compose.managed.yml"), EnvironmentFile: filepath.Join(stateDir, "release.env"), Version: "2.4.0", ReleaseSequence: 17}
	store := recovery.Store{BackupRoot: filepath.Join(t.TempDir(), "backups")}
	db := &database{dump: []byte("database")}
	oldMask := syscall.Umask(0o027)
	defer syscall.Umask(oldMask)
	point, err := store.Create(context.Background(), config, "manual", db)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := store.Restore(context.Background(), config, point.ID, db); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "storage", "shared.txt"))
	if err != nil {
		t.Fatalf("stat restored file: %v", err)
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("restored mode = %o, want 664", info.Mode().Perm())
	}
	info, err = os.Stat(config.EnvironmentFile)
	if err != nil {
		t.Fatalf("stat restored release environment: %v", err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("restored release environment mode = %o, want 660", info.Mode().Perm())
	}
}

func mustWrite(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertContents(t *testing.T, path string, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(contents) != expected {
		t.Fatalf("%s contents = %q, want %q", path, contents, expected)
	}
}
