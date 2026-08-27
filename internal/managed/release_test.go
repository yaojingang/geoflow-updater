package managed_test

import (
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/managed"
)

func TestReleaseRequiresSemanticVersionAndOfficialDigestPins(t *testing.T) {
	t.Parallel()

	release := managed.Release{
		Sequence:        1,
		Version:         "main",
		AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
		WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
		ComposeTemplate: []byte("services: {}\n"),
	}

	if err := release.Validate(); err == nil || !strings.Contains(err.Error(), "semantic") {
		t.Fatalf("Validate() error = %v, want semantic version rejection", err)
	}
}

func TestReleaseRequiresSignedInfrastructureImagesForSupportedDatabaseMajors(t *testing.T) {
	t.Parallel()

	release := managed.Release{
		Sequence:        1,
		Version:         "2.4.0",
		AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
		WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
		PostgresImages:  map[string]string{"18": "pgvector/pgvector@sha256:" + strings.Repeat("3", 64)},
		RedisImages:     map[string]string{"8": "redis@sha256:" + strings.Repeat("4", 64)},
		ComposeTemplate: []byte("services: {}\n"),
	}

	if err := release.Validate(); err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("Validate() error = %v, want missing PostgreSQL 16 rejection", err)
	}
}

func TestReleaseRejectsAVersionDocumentForAnotherRelease(t *testing.T) {
	t.Parallel()

	release := managed.Release{
		Sequence:        1,
		Version:         "2.4.0",
		AppImage:        "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("1", 64),
		WebImage:        "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("2", 64),
		PostgresImages:  map[string]string{"16": "pgvector/pgvector@sha256:" + strings.Repeat("3", 64), "18": "pgvector/pgvector@sha256:" + strings.Repeat("4", 64)},
		RedisImages:     map[string]string{"7": "redis@sha256:" + strings.Repeat("5", 64), "8": "redis@sha256:" + strings.Repeat("6", 64)},
		ComposeTemplate: []byte("services: {}\n"),
		VersionDocument: []byte(`{"version":"2.5.0"}`),
	}

	if err := release.Validate(); err == nil || !strings.Contains(err.Error(), "version document") {
		t.Fatalf("Validate() error = %v, want version document rejection", err)
	}
}
