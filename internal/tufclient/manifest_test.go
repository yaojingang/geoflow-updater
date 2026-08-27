package tufclient_test

import (
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/tufclient"
)

func TestDecodeReleaseManifestAcceptsOnlyPinnedOfficialImagesAndFixedComposeTarget(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 17,
  "version": "2.4.0",
  "source_commit": "` + strings.Repeat("a", 40) + `",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:` + strings.Repeat("1", 64) + `",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:` + strings.Repeat("2", 64) + `",
  "postgres_images": {"16":"pgvector/pgvector@sha256:` + strings.Repeat("3", 64) + `","18":"pgvector/pgvector@sha256:` + strings.Repeat("4", 64) + `"},
  "redis_images": {"7":"redis@sha256:` + strings.Repeat("5", 64) + `","8":"redis@sha256:` + strings.Repeat("6", 64) + `"},
  "compose_target": "deploy/docker-compose.managed.yml",
  "version_target": "releases/2.4.0/version.json"
}`)

	versionDocument := []byte(`{"version":"2.4.0","tag":"v2.4.0"}`)
	release, err := tufclient.DecodeReleaseManifest(manifest, []byte("services: {}\n"), versionDocument)
	if err != nil {
		t.Fatalf("DecodeReleaseManifest() error = %v", err)
	}
	if release.Sequence != 17 || release.MinimumUpdaterProtocol != 2 || release.Version != "2.4.0" || string(release.VersionDocument) != string(versionDocument) {
		t.Fatalf("release = %#v", release)
	}

	legacy := []byte(strings.ReplaceAll(string(manifest), "\"schema_version\": 2,\n  \"minimum_updater_protocol\": 2,", "\"schema_version\": 1,"))
	legacyRelease, err := tufclient.DecodeReleaseManifest(legacy, []byte("services: {}\n"), versionDocument)
	if err != nil || legacyRelease.MinimumUpdaterProtocol != 1 {
		t.Fatalf("legacy release = %#v, error = %v", legacyRelease, err)
	}

	newerProtocol := []byte(strings.Replace(string(manifest), "\"minimum_updater_protocol\": 2", "\"minimum_updater_protocol\": 3", 1))
	if _, err := tufclient.DecodeReleaseManifest(newerProtocol, []byte("services: {}\n"), versionDocument); err == nil || !strings.Contains(err.Error(), "current protocol") {
		t.Fatalf("newer protocol error = %v", err)
	}

	for name, incomplete := range map[string][]byte{
		"source commit":  []byte(strings.Replace(string(manifest), "  \"source_commit\": \""+strings.Repeat("a", 40)+"\",\n", "", 1)),
		"version target": []byte(strings.Replace(string(manifest), ",\n  \"version_target\": \"releases/2.4.0/version.json\"", "", 1)),
	} {
		if _, err := tufclient.DecodeReleaseManifest(incomplete, []byte("services: {}\n"), versionDocument); err == nil {
			t.Fatalf("Phase C manifest without %s was accepted", name)
		}
	}
}

func TestDecodeReleaseManifestRejectsMutableImageTags(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
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
  "schema_version": 2,
  "minimum_updater_protocol": 2,
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

func TestDecodeReleaseManifestRejectsAMutableSourceReference(t *testing.T) {
	t.Parallel()

	manifest := []byte(`{
  "schema_version": 2,
  "minimum_updater_protocol": 2,
  "release_sequence": 17,
  "version": "2.4.0",
  "source_commit": "main",
  "app_image": "ghcr.io/yaojingang/geoflow-app@sha256:` + strings.Repeat("1", 64) + `",
  "web_image": "ghcr.io/yaojingang/geoflow-web@sha256:` + strings.Repeat("2", 64) + `",
  "postgres_images": {"16":"pgvector/pgvector@sha256:` + strings.Repeat("3", 64) + `","18":"pgvector/pgvector@sha256:` + strings.Repeat("4", 64) + `"},
  "redis_images": {"7":"redis@sha256:` + strings.Repeat("5", 64) + `","8":"redis@sha256:` + strings.Repeat("6", 64) + `"},
  "compose_target": "deploy/docker-compose.managed.yml"
}`)

	if _, err := tufclient.DecodeReleaseManifest(manifest, []byte("services: {}\n")); err == nil {
		t.Fatal("DecodeReleaseManifest() accepted a mutable source reference")
	}
}
