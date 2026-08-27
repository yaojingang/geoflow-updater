package bootstrap

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/secure-systems-lab/go-securesystemslib/cjson"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	digestPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion   int              `json:"schema_version"`
	ReleaseSequence uint64           `json:"release_sequence"`
	UpdaterVersion  string           `json:"updater_version"`
	Expires         time.Time        `json:"expires"`
	Assets          map[string]Asset `json:"assets"`
}

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type Envelope struct {
	Signed     Manifest    `json:"signed"`
	Signatures []Signature `json:"signatures"`
}

func Sign(manifest Manifest, privateKey ed25519.PrivateKey) (Envelope, error) {
	if err := manifest.Validate(time.Time{}); err != nil {
		return Envelope{}, err
	}
	canonical, err := cjson.EncodeCanonical(manifest)
	if err != nil {
		return Envelope{}, fmt.Errorf("canonicalize bootstrap manifest: %w", err)
	}
	key, err := metadata.KeyFromPublicKey(privateKey.Public())
	if err != nil {
		return Envelope{}, fmt.Errorf("describe signing key: %w", err)
	}
	keyID, err := key.ID()
	if err != nil {
		return Envelope{}, fmt.Errorf("calculate signing key id: %w", err)
	}

	return Envelope{
		Signed: manifest,
		Signatures: []Signature{{
			KeyID: keyID,
			Sig:   hex.EncodeToString(ed25519.Sign(privateKey, canonical)),
		}},
	}, nil
}

func Verify(envelope Envelope, trustedRoot []byte, now time.Time) error {
	if err := envelope.Signed.Validate(now); err != nil {
		return err
	}
	root, err := metadata.Root().FromBytes(trustedRoot)
	if err != nil {
		return fmt.Errorf("decode trusted root: %w", err)
	}
	role, ok := root.Signed.Roles["targets"]
	if !ok || role.Threshold < 1 {
		return errors.New("trusted root has no targets role")
	}
	canonical, err := cjson.EncodeCanonical(envelope.Signed)
	if err != nil {
		return fmt.Errorf("canonicalize bootstrap manifest: %w", err)
	}

	authorized := make(map[string]struct{}, len(role.KeyIDs))
	for _, keyID := range role.KeyIDs {
		authorized[keyID] = struct{}{}
	}
	verified := make(map[string]struct{})
	for _, candidate := range envelope.Signatures {
		if _, ok := authorized[candidate.KeyID]; !ok {
			continue
		}
		key, ok := root.Signed.Keys[candidate.KeyID]
		if !ok {
			continue
		}
		publicKey, err := key.ToPublicKey()
		if err != nil {
			continue
		}
		ed25519Key, ok := publicKey.(ed25519.PublicKey)
		if !ok {
			continue
		}
		signature, err := hex.DecodeString(candidate.Sig)
		if err != nil || !ed25519.Verify(ed25519Key, canonical, signature) {
			continue
		}
		verified[candidate.KeyID] = struct{}{}
	}
	if len(verified) < role.Threshold {
		return fmt.Errorf("bootstrap signature threshold not met: got %d, want %d", len(verified), role.Threshold)
	}

	return nil
}

func (manifest Manifest) Validate(now time.Time) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported bootstrap manifest schema %d", manifest.SchemaVersion)
	}
	if manifest.ReleaseSequence == 0 {
		return errors.New("bootstrap release sequence is required")
	}
	if !versionPattern.MatchString(manifest.UpdaterVersion) {
		return errors.New("updater version is invalid")
	}
	if manifest.Expires.IsZero() {
		return errors.New("bootstrap manifest expiry is required")
	}
	if !now.IsZero() && !now.Before(manifest.Expires) {
		return errors.New("bootstrap manifest has expired")
	}
	if len(manifest.Assets) == 0 {
		return errors.New("bootstrap manifest has no assets")
	}
	for platform, asset := range manifest.Assets {
		if platform != "linux-amd64" && platform != "linux-arm64" {
			return fmt.Errorf("unsupported bootstrap platform %q", platform)
		}
		expectedPrefix := "https://github.com/yaojingang/geoflow-updater/releases/download/v" + manifest.UpdaterVersion + "/"
		if !strings.HasPrefix(asset.URL, expectedPrefix) || strings.Contains(strings.TrimPrefix(asset.URL, expectedPrefix), "/") {
			return fmt.Errorf("asset URL for %s is outside the official release", platform)
		}
		if !digestPattern.MatchString(asset.SHA256) {
			return fmt.Errorf("asset digest for %s is invalid", platform)
		}
		if asset.Size < 1 || asset.Size > 100*1024*1024 {
			return fmt.Errorf("asset size for %s is outside the allowed range", platform)
		}
	}

	return nil
}
