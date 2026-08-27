package recovery_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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
	mustWrite(t, config.EnvironmentFile, []byte("new release\n"), 0o640)

	if err := store.Restore(context.Background(), config, point.ID, db); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	assertContents(t, filepath.Join(root, ".env.prod"), "APP_ENV=production\n")
	assertContents(t, filepath.Join(root, "version.json"), "{\"version\":\"2.4.0\"}\n")
	assertContents(t, filepath.Join(root, "storage", "app", "customer.txt"), "customer data")
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
