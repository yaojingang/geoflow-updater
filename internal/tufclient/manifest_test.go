package tufclient_test

import (
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/tufclient"
)

func TestDecodeReleaseManifestAcceptsOnlyPinnedOfficialImagesAndFixedComposeTarget(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 1,
  "release_sequence": 17,
  "version": "2.4.0",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:` + strings.Repeat("1", 64) + `",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:` + strings.Repeat("2", 64) + `",
  "postgres_images": {"16":"pgvector/pgvector@sha256:` + strings.Repeat("3", 64) + `","18":"pgvector/pgvector@sha256:` + strings.Repeat("4", 64) + `"},
  "redis_images": {"7":"redis@sha256:` + strings.Repeat("5", 64) + `","8":"redis@sha256:` + strings.Repeat("6", 64) + `"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`)

	release, err := tufclient.DecodeReleaseManifest(manifest, []byte("services: {}\n"))
	if err != nil {
		t.Fatalf("DecodeReleaseManifest() error = %v", err)
	}
	if release.Sequence != 17 || release.Version != "2.4.0" {
		t.Fatalf("release = %#v", release)
	}
}

func TestDecodeReleaseManifestRejectsMutableImageTags(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 1,
  "release_sequence": 17,
  "version": "2.4.0",
  "app_image": "ghcr.io/yaojingang/geoflow-app:latest",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:` + strings.Repeat("2", 64) + `",
  "postgres_images": {"16":"pgvector/pgvector@sha256:` + strings.Repeat("3", 64) + `","18":"pgvector/pgvector@sha256:` + strings.Repeat("4", 64) + `"},
  "redis_images": {"7":"redis@sha256:` + strings.Repeat("5", 64) + `","8":"redis@sha256:` + strings.Repeat("6", 64) + `"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`)

	if _, err := tufclient.DecodeReleaseManifest(manifest, []byte("services: {}\n")); err == nil {
		t.Fatal("DecodeReleaseManifest() accepted a mutable image tag")
	}
}

func TestDecodeReleaseManifestRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 1,
  "release_sequence": 17,
  "version": "2.4.0",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:` + strings.Repeat("1", 64) + `",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:` + strings.Repeat("2", 64) + `",
  "postgres_images": {"16":"pgvector/pgvector@sha256:` + strings.Repeat("3", 64) + `","18":"pgvector/pgvector@sha256:` + strings.Repeat("4", 64) + `"},
  "redis_images": {"7":"redis@sha256:` + strings.Repeat("5", 64) + `","8":"redis@sha256:` + strings.Repeat("6", 64) + `"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "command": "rm -rf /"
}`)

	if _, err := tufclient.DecodeReleaseManifest(manifest, []byte("services: {}\n")); err == nil {
		t.Fatal("DecodeReleaseManifest() accepted an unknown field")
	}
}
