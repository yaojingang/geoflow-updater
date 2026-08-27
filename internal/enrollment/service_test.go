package enrollment_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type releaseResolver struct {
	release managed.Release
}

func (resolver releaseResolver) Current(context.Context) (managed.Release, error) {
	return resolver.release, nil
}

func TestEnrollWritesPinnedManagedDeploymentAndPrivateControlToken(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), validEnvironment()+"PGVECTOR_IMAGE=pgvector/pgvector:pg16\nREDIS_IMAGE=redis:7-alpine\nPOSTGRES_DATA_DIR=./docker-data/prod/postgres\nPOSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql/data\n")
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))
	mustMkdir(t, filepath.Join(root, "docker-data", "prod", "postgres"))
	mustWriteFile(t, filepath.Join(root, "docker-data", "prod", "postgres", "PG_VERSION"), "16\n")

	stateDir := filepath.Join(t.TempDir(), "state")
	service := enrollment.Service{
		StateDir: stateDir,
		ControlGroupID: func() (int, error) {
			return 991, nil
		},
		Chown: func(string, int, int) error { return nil },
		Now: func() time.Time {
			return time.Date(2026, time.August, 27, 9, 30, 0, 0, time.UTC)
		},
		Random: strings.NewReader(strings.Repeat("a", 32)),
		Releases: releaseResolver{release: managed.Release{
			Sequence: 17,
			Version:  "2.4.0",
			AppImage: "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
			WebImage: "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
			PostgresImages: map[string]string{
				"16": "pgvector/pgvector@sha256:" + strings.Repeat("3", 64),
				"18": "pgvector/pgvector@sha256:" + strings.Repeat("4", 64),
			},
			RedisImages: map[string]string{
				"7": "redis@sha256:" + strings.Repeat("5", 64),
				"8": "redis@sha256:" + strings.Repeat("6", 64),
			},
			ComposeTemplate: []byte("name: geoflow-${GEOFLOW_INSTANCE_ID}\nservices:\n  app:\n    image: ${GEOFLOW_APP_IMAGE}\n"),
		}},
	}

	result, err := service.Enroll(context.Background(), enrollment.Request{
		InstanceID: "primary",
		Root:       root,
	})
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}

	if result.Instance.ID != "primary" {
		t.Fatalf("instance id = %q, want primary", result.Instance.ID)
	}
	if result.Instance.ReleaseSequence != 17 {
		t.Fatalf("release sequence = %d, want 17", result.Instance.ReleaseSequence)
	}

	instanceDir := filepath.Join(stateDir, "instances", "primary")
	assertFileContains(t, filepath.Join(instanceDir, "instance.yml"), "release_sequence: 17")
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_APP_IMAGE=ghcr.io/yaojingang/geoflow-app@sha256:")
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_UPDATER_GROUP_ID=991")
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_POSTGRES_IMAGE=pgvector/pgvector@sha256:"+strings.Repeat("3", 64))
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_REDIS_IMAGE=redis@sha256:"+strings.Repeat("5", 64))
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_COMPOSE_PROJECT_NAME=geoflow-laravel-prod")
	expectedDataDir, err := filepath.EvalSymlinks(filepath.Join(root, "docker-data", "prod", "postgres"))
	if err != nil {
		t.Fatalf("resolve expected PostgreSQL data directory: %v", err)
	}
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_POSTGRES_DATA_DIR="+strconv.Quote(expectedDataDir))
	assertFileContains(t, filepath.Join(instanceDir, "release.env"), "GEOFLOW_POSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql/data")
	assertFileContains(t, filepath.Join(instanceDir, "docker-compose.managed.yml"), "image: ${GEOFLOW_APP_IMAGE}")

	controlTokenPath := filepath.Join(instanceDir, "control.token")
	info, err := os.Stat(controlTokenPath)
	if err != nil {
		t.Fatalf("stat control token: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("control token mode = %o, want 640", info.Mode().Perm())
	}
	if result.ControlToken != "" {
		t.Fatal("enrollment result exposed the control token")
	}
}

func TestEnrollRejectsSymlinkedInstanceRoot(t *testing.T) {
	t.Parallel()

	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "geoflow")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	service := enrollment.Service{StateDir: filepath.Join(t.TempDir(), "state")}
	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: linkRoot})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Enroll() error = %v, want symbolic link rejection", err)
	}
}

func TestEnrollRejectsSymlinkedRequiredLayoutPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realEnvironment := filepath.Join(t.TempDir(), ".env.prod")
	mustWriteFile(t, realEnvironment, validEnvironment())
	if err := os.Symlink(realEnvironment, filepath.Join(root, ".env.prod")); err != nil {
		t.Fatalf("create environment symlink: %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))

	service := enrollment.Service{StateDir: filepath.Join(t.TempDir(), "state")}
	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil || !strings.Contains(err.Error(), ".env.prod") {
		t.Fatalf("Enroll() error = %v, want symlinked environment rejection", err)
	}
}

func TestEnrollRejectsUnboundedInstalledVersionDocument(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), validEnvironment())
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0","padding":"`+strings.Repeat("a", 1024*1024)+`"}`)
	mustMkdir(t, filepath.Join(root, "storage"))

	service := enrollment.Service{StateDir: filepath.Join(t.TempDir(), "state")}
	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil || !strings.Contains(err.Error(), "version.json") {
		t.Fatalf("Enroll() error = %v, want oversized version document rejection", err)
	}
}

func TestEnrollRejectsASecondInstanceDuringSingleHostPhaseA(t *testing.T) {
	t.Parallel()

	service := enrollment.Service{StateDir: filepath.Join(t.TempDir(), "state")}
	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "secondary", Root: "/opt/geoflow"})
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("Enroll() error = %v, want primary instance rejection", err)
	}
}

func TestEnrollRemovesStagedStateWhenCredentialGenerationFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), validEnvironment()+"GEOFLOW_UPDATER_POSTGRES_MAJOR=18\nGEOFLOW_UPDATER_REDIS_MAJOR=8\nPOSTGRES_DATA_DIR=./docker-data/prod/postgres\nPOSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql\n")
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))
	mustMkdir(t, filepath.Join(root, "docker-data", "prod", "postgres", "18", "docker"))
	mustWriteFile(t, filepath.Join(root, "docker-data", "prod", "postgres", "18", "docker", "PG_VERSION"), "18\n")
	stateDir := t.TempDir()
	service := enrollment.Service{
		StateDir: stateDir,
		Random:   strings.NewReader("short"),
		Releases: releaseResolver{release: managed.Release{
			Sequence:        17,
			Version:         "2.4.0",
			AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
			WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
			PostgresImages:  supportedPostgresImages(),
			RedisImages:     supportedRedisImages(),
			ComposeTemplate: []byte("services: {}\n"),
		}},
	}

	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil {
		t.Fatal("Enroll() succeeded with insufficient random data")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "instances", "primary")); !os.IsNotExist(statErr) {
		t.Fatalf("partial instance state remains after failure: %v", statErr)
	}
}

func TestEnrollRejectsAReleaseThatWouldUpgradeTheDatabaseWithoutABackupTransaction(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), "APP_ENV=production\nGEOFLOW_UPDATER_POSTGRES_MAJOR=18\nGEOFLOW_UPDATER_REDIS_MAJOR=8\n")
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.3.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))
	service := enrollment.Service{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Releases: releaseResolver{release: managed.Release{
			Sequence:        17,
			Version:         "2.4.0",
			AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
			WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
			PostgresImages:  supportedPostgresImages(),
			RedisImages:     supportedRedisImages(),
			ComposeTemplate: []byte("services: {}\n"),
		}},
	}

	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil || !strings.Contains(err.Error(), "bridge release") {
		t.Fatalf("Enroll() error = %v, want bridge release mismatch rejection", err)
	}
}

func TestEnrollRejectsAnUnknownExistingDatabaseMajor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), validEnvironment()+"PGVECTOR_IMAGE=pgvector/pgvector:pg17\nREDIS_IMAGE=redis:8-alpine\n")
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))
	service := enrollment.Service{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Releases: releaseResolver{release: managed.Release{
			Sequence:        17,
			Version:         "2.4.0",
			AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
			WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
			PostgresImages:  supportedPostgresImages(),
			RedisImages:     supportedRedisImages(),
			ComposeTemplate: []byte("services: {}\n"),
		}},
	}

	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL major") {
		t.Fatalf("Enroll() error = %v, want unsupported PostgreSQL major rejection", err)
	}
}

func TestEnrollRejectsMissingInfrastructureMajorInsteadOfGuessingAgainstExistingData(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".env.prod"), validEnvironment())
	mustWriteFile(t, filepath.Join(root, "version.json"), `{"version":"2.4.0"}`)
	mustMkdir(t, filepath.Join(root, "storage"))
	service := enrollment.Service{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Releases: releaseResolver{release: managed.Release{
			Sequence:        17,
			Version:         "2.4.0",
			AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
			WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
			PostgresImages:  supportedPostgresImages(),
			RedisImages:     supportedRedisImages(),
			ComposeTemplate: []byte("services: {}\n"),
		}},
	}

	_, err := service.Enroll(context.Background(), enrollment.Request{InstanceID: "primary", Root: root})
	if err == nil || !strings.Contains(err.Error(), "cannot be inferred") {
		t.Fatalf("Enroll() error = %v, want missing infrastructure major rejection", err)
	}
}

func supportedPostgresImages() map[string]string {
	return map[string]string{
		"16": "pgvector/pgvector@sha256:" + strings.Repeat("3", 64),
		"18": "pgvector/pgvector@sha256:" + strings.Repeat("4", 64),
	}
}

func supportedRedisImages() map[string]string {
	return map[string]string{
		"7": "redis@sha256:" + strings.Repeat("5", 64),
		"8": "redis@sha256:" + strings.Repeat("6", 64),
	}
}

func validEnvironment() string {
	return "APP_ENV=production\n" +
		"APP_KEY=base64:" + strings.Repeat("A", 43) + "=\n" +
		"DB_CONNECTION=pgsql\n" +
		"DB_HOST=postgres\n" +
		"DB_PASSWORD=test-only\n" +
		"REDIS_HOST=redis\n"
}

func mustWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func assertFileContains(t *testing.T, path string, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(contents), expected) {
		t.Fatalf("%s does not contain %q", path, expected)
	}
}
