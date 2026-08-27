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
	mustWrite(t, filepath.Join(targetsDir, "releases", "2.4.0", "version.json"), []byte(`{"version":"2.4.0"}`))
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 17,
  "version": "2.4.0",
  "source_commit": "`+strings.Repeat("b", 40)+`",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("1", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("2", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/2.4.0/version.json"
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

	mustWrite(t, filepath.Join(targetsDir, "releases", "2.4.1", "version.json"), []byte(`{"version":"2.4.1"}`))
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 18,
  "version": "2.4.1",
  "source_commit": "`+strings.Repeat("b", 40)+`",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("3", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("4", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/2.4.1/version.json"
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
	mustWrite(t, filepath.Join(targetsDir, "releases", "2.4.2", "version.json"), []byte(`{"version":"2.4.2"}`))
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 17,
  "version": "2.4.2",
  "source_commit": "`+strings.Repeat("b", 40)+`",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("5", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("6", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/2.4.2/version.json"
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
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 18,
  "version": "2.4.1",
  "source_commit": "`+strings.Repeat("b", 40)+`",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:`+strings.Repeat("3", 64)+`",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:`+strings.Repeat("4", 64)+`",
  "postgres_images": {"16":"pgvector/pgvector@sha256:`+strings.Repeat("7", 64)+`","18":"pgvector/pgvector@sha256:`+strings.Repeat("8", 64)+`"},
  "redis_images": {"7":"redis@sha256:`+strings.Repeat("9", 64)+`","8":"redis@sha256:`+strings.Repeat("a", 64)+`"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/2.4.1/version.json"
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

func TestPublishSkipsOrphanedImmutableMetadataVersions(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	keysDir := filepath.Join(workingDir, "keys")
	repositoryDir := filepath.Join(workingDir, "repository")
	targetsDir := filepath.Join(workingDir, "targets")
	mustWrite(t, filepath.Join(targetsDir, "deploy", "docker-compose.managed.yml"), []byte("services: {}\n"))
	mustWriteReleaseManifest(t, targetsDir, 1, "1.0.0", "1", "2")
	if err := tufrepo.Initialize(tufrepo.InitializeOptions{
		KeysDir:       keysDir,
		RepositoryDir: repositoryDir,
		TargetsDir:    targetsDir,
		Now:           func() time.Time { return time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	metadataDir := filepath.Join(repositoryDir, "metadata")
	mustWrite(t, filepath.Join(metadataDir, "2.targets.json"), []byte("interrupted publication"))
	mustWrite(t, filepath.Join(metadataDir, "2.snapshot.json"), []byte("interrupted publication"))
	mustWriteReleaseManifest(t, targetsDir, 2, "1.1.0", "3", "4")

	if err := tufrepo.Publish(tufrepo.PublishOptions{
		RepositoryDir:    repositoryDir,
		TargetsDir:       targetsDir,
		TargetsKeyPath:   filepath.Join(keysDir, "targets.pem"),
		SnapshotKeyPath:  filepath.Join(keysDir, "snapshot.pem"),
		TimestampKeyPath: filepath.Join(keysDir, "timestamp.pem"),
		Now:              func() time.Time { return time.Date(2026, time.August, 28, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("Publish() after orphaned metadata error = %v", err)
	}

	timestamp, err := metadata.Timestamp().FromFile(filepath.Join(metadataDir, "timestamp.json"))
	if err != nil {
		t.Fatalf("load timestamp metadata: %v", err)
	}
	if timestamp.Signed.Meta["snapshot.json"].Version != 3 {
		t.Fatalf("published snapshot version = %d, want 3", timestamp.Signed.Meta["snapshot.json"].Version)
	}
	snapshot, err := metadata.Snapshot().FromFile(filepath.Join(metadataDir, "3.snapshot.json"))
	if err != nil {
		t.Fatalf("load recovered snapshot metadata: %v", err)
	}
	if snapshot.Signed.Meta["targets.json"].Version != 3 {
		t.Fatalf("published targets version = %d, want 3", snapshot.Signed.Meta["targets.json"].Version)
	}
}

func mustWriteReleaseManifest(t *testing.T, targetsDir string, sequence int, version string, appDigest string, webDigest string) {
	t.Helper()
	mustWrite(t, filepath.Join(targetsDir, "releases", version, "version.json"), []byte(fmt.Sprintf(`{"version":%q}`, version)))
	mustWrite(t, filepath.Join(targetsDir, "releases", "current.json"), []byte(fmt.Sprintf(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": %d,
  "version": %q,
  "source_commit": "%s",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:%s",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:%s",
  "postgres_images": {"16":"pgvector/pgvector@sha256:%s","18":"pgvector/pgvector@sha256:%s"},
  "redis_images": {"7":"redis@sha256:%s","8":"redis@sha256:%s"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/%s/version.json"
}`,
		sequence,
		version,
		strings.Repeat("b", 40),
		strings.Repeat(appDigest, 64),
		strings.Repeat(webDigest, 64),
		strings.Repeat("5", 64),
		strings.Repeat("6", 64),
		strings.Repeat("7", 64),
		strings.Repeat("8", 64),
		version,
	)))
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
