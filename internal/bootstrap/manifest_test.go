package bootstrap_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/yaojingang/geoflow-updater/internal/bootstrap"
)

func TestManifestSignatureIsAuthorizedByTrustedRootTargetsRole(t *testing.T) {
	t.Parallel()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	root := metadata.Root(time.Now().Add(24 * time.Hour))
	key, err := metadata.KeyFromPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("convert key: %v", err)
	}
	if err := root.Signed.AddKey(key, "targets"); err != nil {
		t.Fatalf("authorize targets key: %v", err)
	}
	rootBytes, err := root.ToBytes(true)
	if err != nil {
		t.Fatalf("encode root: %v", err)
	}

	envelope, err := bootstrap.Sign(bootstrap.Manifest{
		SchemaVersion:  1,
		UpdaterVersion: "0.1.0",
		Expires:        time.Date(2026, time.September, 27, 0, 0, 0, 0, time.UTC),
		Assets: map[string]bootstrap.Asset{
			"linux-amd64": {
				URL:    "https://github.com/yaojingang/geoflow-updater/releases/download/v0.1.0/geoflow-updater_0.1.0_linux_amd64.tar.gz",
				SHA256: strings.Repeat("a", 64),
				Size:   12345,
			},
		},
	}, privateKey)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if err := bootstrap.Verify(envelope, rootBytes, time.Date(2026, time.August, 28, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	envelope.Signed.Assets["linux-amd64"] = bootstrap.Asset{
		URL:    envelope.Signed.Assets["linux-amd64"].URL,
		SHA256: strings.Repeat("b", 64),
		Size:   12345,
	}
	if err := bootstrap.Verify(envelope, rootBytes, time.Date(2026, time.August, 28, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("Verify() accepted a tampered manifest")
	}
}
