package tufrepo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/yaojingang/geoflow-updater/internal/tufclient"
	"github.com/yaojingang/geoflow-updater/internal/tufrepo"
)

func TestInitializeCreatesTwoOfThreeRootAndConsumableRepository(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	keysDir := filepath.Join(workingDir, "offline-and-online-keys")
	repositoryDir := filepath.Join(workingDir, "repository")
	targetsDir := filepath.Join(workingDir, "source-targets")
	mustWrite(t, filepath.Join(targetsDir, "deploy", "docker-compose.managed.yml"), []byte("services: {}\n"))
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 1,
  "release_sequence": 17,
  "version": "2.4.0",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("1", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("2", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`))

	err := tufrepo.Initialize(tufrepo.InitializeOptions{
		KeysDir:       keysDir,
		RepositoryDir: repositoryDir,
		TargetsDir:    targetsDir,
		Now:           func() time.Time { return time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	root, err := metadata.Root().FromFile(filepath.Join(repositoryDir, "metadata", "root.json"))
	if err != nil {
		t.Fatalf("load root metadata: %v", err)
	}
	rootRole := root.Signed.Roles["root"]
	if rootRole.Threshold != 2 || len(rootRole.KeyIDs) != 3 {
		t.Fatalf("root role = %#v, want two-of-three", rootRole)
	}
	for index := 1; index <= 3; index++ {
		assertPrivateFile(t, filepath.Join(keysDir, "root-"+string(rune('0'+index))+".pem"))
	}
	assertNoPrivateKeys(t, repositoryDir)

	server := httptest.NewServer(http.FileServer(http.Dir(repositoryDir)))
	defer server.Close()
	rootBytes, err := os.ReadFile(filepath.Join(repositoryDir, "metadata", "root.json"))
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	release, err := (tufclient.Client{
		MetadataURL: server.URL + "/metadata",
		TargetsURL:  server.URL + "/targets",
		CacheDir:    filepath.Join(workingDir, "client-cache"),
		TrustedRoot: rootBytes,
	}).Current(context.Background())
	if err != nil {
		t.Fatalf("TUF client Current() error = %v", err)
	}
	if release.Sequence != 17 || release.Version != "2.4.0" {
		t.Fatalf("release = %#v", release)
	}

	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 1,
  "release_sequence": 18,
  "version": "2.4.1",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("3", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("4", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`))
	if err := tufrepo.Publish(tufrepo.PublishOptions{
		RepositoryDir:    repositoryDir,
		TargetsDir:       targetsDir,
		TargetsKeyPath:   filepath.Join(keysDir, "targets.pem"),
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
		Now:              func() time.Time { return time.Date(2026, time.August, 28, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	release, err = (tufclient.Client{
		MetadataURL: server.URL + "/metadata",
		TargetsURL:  server.URL + "/targets",
		CacheDir:    filepath.Join(workingDir, "second-client-cache"),
		TrustedRoot: rootBytes,
	}).Current(context.Background())
	if err != nil {
		t.Fatalf("TUF client Current() after publish error = %v", err)
	}
	if release.Sequence != 18 || release.Version != "2.4.1" {
		t.Fatalf("published release = %#v", release)
	}
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 1,
  "release_sequence": 17,
  "version": "2.4.2",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("5", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("6", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`))
	rollbackErr := tufrepo.Publish(tufrepo.PublishOptions{
		RepositoryDir:    repositoryDir,
		TargetsDir:       targetsDir,
		TargetsKeyPath:   filepath.Join(keysDir, "targets.pem"),
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
		Now:              func() time.Time { return time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC) },
	})
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "greater than published sequence 18") {
		t.Fatalf("Publish() rollback error = %v", rollbackErr)
	}
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 1,
  "release_sequence": 18,
  "version": "2.4.1",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("3", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("4", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`))

	if err := tufrepo.RefreshOnline(tufrepo.RefreshOnlineOptions{
		RepositoryDir:    repositoryDir,
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
		Now:              func() time.Time { return time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("RefreshOnline() error = %v", err)
	}
	release, err = (tufclient.Client{
		MetadataURL: server.URL + "/metadata",
		TargetsURL:  server.URL + "/targets",
		CacheDir:    filepath.Join(workingDir, "third-client-cache"),
		TrustedRoot: rootBytes,
	}).Current(context.Background())
	if err != nil {
		t.Fatalf("TUF client Current() after refresh error = %v", err)
	}
	if release.Sequence != 18 || release.Version != "2.4.1" {
		t.Fatalf("refreshed release = %#v", release)
	}
	refreshedTimestamp, err := metadata.Timestamp().FromFile(filepath.Join(repositoryDir, "metadata", "timestamp.json"))
	if err != nil {
		t.Fatalf("load refreshed timestamp: %v", err)
	}
	if refreshedTimestamp.Signed.Version != 3 {
		t.Fatalf("refreshed timestamp version = %d, want 3", refreshedTimestamp.Signed.Version)
	}
	currentSnapshot, err := metadata.Snapshot().FromFile(filepath.Join(repositoryDir, "metadata", "3.snapshot.json"))
	if err != nil {
		t.Fatalf("load online-refreshed snapshot metadata: %v", err)
	}
	if currentSnapshot.Signed.Meta["targets.json"].Version != 2 {
		t.Fatalf("online refresh changed targets version to %d, want 2", currentSnapshot.Signed.Meta["targets.json"].Version)
	}

	if err := tufrepo.Refresh(tufrepo.RefreshOptions{
		RepositoryDir:    repositoryDir,
		TargetsKeyPath:   filepath.Join(keysDir, "targets.pem"),
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
		Now:              func() time.Time { return time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	currentSnapshot, err = metadata.Snapshot().FromFile(filepath.Join(repositoryDir, "metadata", "4.snapshot.json"))
	if err != nil {
		t.Fatalf("load current snapshot metadata: %v", err)
	}
	currentTargetsVersion := currentSnapshot.Signed.Meta["targets.json"].Version
	currentTargetsPath := filepath.Join(repositoryDir, "metadata", fmt.Sprintf("%d.targets.json", currentTargetsVersion))
	currentTargetsBytes, err := os.ReadFile(currentTargetsPath)
	if err != nil {
		t.Fatalf("read current targets metadata: %v", err)
	}
	if err := os.WriteFile(currentTargetsPath, append(currentTargetsBytes, '\n'), 0o644); err != nil {
		t.Fatalf("tamper current targets metadata: %v", err)
	}
	tamperErr := tufrepo.RefreshOnline(tufrepo.RefreshOnlineOptions{
		RepositoryDir:    repositoryDir,
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
	})
	if tamperErr == nil || !strings.Contains(tamperErr.Error(), "targets metadata file") {
		t.Fatalf("RefreshOnline() tamper error = %v", tamperErr)
	}
}

func mustWrite(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir target parent: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o640); err != nil {
		t.Fatalf("write target: %v", err)
	}
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o, want 600", info.Mode().Perm())
	}
}

func assertNoPrivateKeys(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".pem") {
			t.Fatalf("private key leaked into repository: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect repository for private keys: %v", err)
	}
}
