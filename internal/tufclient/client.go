package tufclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

const (
	ReleaseManifestTarget = "releases/current.json"
	ManagedComposeTarget  = "deploy/docker-compose.managed.yml"
)

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var sourceCommitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

type releaseManifest struct {
	SchemaVersion   int               `json:"schema_version"`
	ReleaseSequence uint64            `json:"release_sequence"`
	Version         string            `json:"version"`
	SourceCommit    string            `json:"source_commit,omitempty"`
	AppImage        string            `json:"app_image"`
	WebImage        string            `json:"web_image"`
	PostgresImages  map[string]string `json:"postgres_images"`
	RedisImages     map[string]string `json:"redis_images"`
	ComposeTarget   string            `json:"compose_target"`
	VersionTarget   string            `json:"version_target,omitempty"`
}

type Client struct {
	MetadataURL string
	TargetsURL  string
	CacheDir    string
	TrustedRoot []byte
	HTTPClient  *http.Client
}

func (client Client) Current(ctx context.Context) (managed.Release, error) {
	if err := ctx.Err(); err != nil {
		return managed.Release{}, err
	}
	if client.MetadataURL == "" || client.TargetsURL == "" {
		return managed.Release{}, errors.New("official TUF metadata and target URLs are required")
	}
	if len(client.TrustedRoot) == 0 {
		return managed.Release{}, errors.New("embedded trusted root metadata is required")
	}

	configuration, err := config.New(client.MetadataURL, client.TrustedRoot)
	if err != nil {
		return managed.Release{}, fmt.Errorf("configure TUF client: %w", err)
	}
	configuration.LocalMetadataDir = filepath.Join(client.CacheDir, "metadata")
	configuration.LocalTargetsDir = filepath.Join(client.CacheDir, "targets")
	configuration.RemoteTargetsURL = client.TargetsURL
	configuration.PrefixTargetsWithHash = true
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: sameOriginRedirects(client.MetadataURL, client.TargetsURL),
		}
	}
	if err := configuration.SetDefaultFetcherHTTPClient(httpClient); err != nil {
		return managed.Release{}, fmt.Errorf("configure TUF transport: %w", err)
	}

	trustedUpdater, err := updater.New(configuration)
	if err != nil {
		return managed.Release{}, fmt.Errorf("initialize TUF updater: %w", err)
	}
	if err := trustedUpdater.Refresh(); err != nil {
		return managed.Release{}, fmt.Errorf("refresh TUF metadata: %w", err)
	}

	manifestBytes, err := downloadTarget(trustedUpdater, ReleaseManifestTarget, 1024*1024)
	if err != nil {
		return managed.Release{}, err
	}
	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return managed.Release{}, err
	}
	composeBytes, err := downloadTarget(trustedUpdater, manifest.ComposeTarget, 2*1024*1024)
	if err != nil {
		return managed.Release{}, err
	}

	var versionBytes []byte
	if manifest.VersionTarget != "" {
		versionBytes, err = downloadTarget(trustedUpdater, manifest.VersionTarget, 1024*1024)
		if err != nil {
			return managed.Release{}, err
		}
	}

	return DecodeReleaseManifest(manifestBytes, composeBytes, versionBytes)
}

func sameOriginRedirects(baseURLs ...string) func(*http.Request, []*http.Request) error {
	allowed := make(map[string]struct{}, len(baseURLs))
	for _, baseURL := range baseURLs {
		request, err := http.NewRequest(http.MethodGet, baseURL, nil)
		if err == nil {
			allowed[request.URL.Scheme+"://"+request.URL.Host] = struct{}{}
		}
	}

	return func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("TUF redirect limit exceeded")
		}
		if _, ok := allowed[request.URL.Scheme+"://"+request.URL.Host]; !ok {
			return errors.New("TUF redirect left the configured repository origin")
		}

		return nil
	}
}

func DecodeReleaseManifest(manifestBytes []byte, composeBytes []byte, versionDocuments ...[]byte) (managed.Release, error) {
	manifest, err := decodeManifest(manifestBytes)
	if err != nil {
		return managed.Release{}, err
	}
	release := managed.Release{
		Sequence:        manifest.ReleaseSequence,
		Version:         manifest.Version,
		SourceCommit:    manifest.SourceCommit,
		AppImage:        manifest.AppImage,
		WebImage:        manifest.WebImage,
		PostgresImages:  manifest.PostgresImages,
		RedisImages:     manifest.RedisImages,
		ComposeTemplate: composeBytes,
	}
	if len(versionDocuments) > 1 {
		return managed.Release{}, errors.New("only one signed version document is allowed")
	}
	if len(versionDocuments) == 1 {
		release.VersionDocument = versionDocuments[0]
	}
	if err := release.Validate(); err != nil {
		return managed.Release{}, fmt.Errorf("validate release manifest: %w", err)
	}

	return release, nil
}

func decodeManifest(contents []byte) (releaseManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest releaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return releaseManifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return releaseManifest{}, err
	}
	if manifest.SchemaVersion != 1 {
		return releaseManifest{}, fmt.Errorf("unsupported release manifest schema %d", manifest.SchemaVersion)
	}
	if manifest.ReleaseSequence == 0 || !releaseVersionPattern.MatchString(manifest.Version) {
		return releaseManifest{}, errors.New("release manifest sequence or version is invalid")
	}
	if manifest.SourceCommit != "" && !sourceCommitPattern.MatchString(manifest.SourceCommit) {
		return releaseManifest{}, errors.New("release manifest source commit is invalid")
	}
	if manifest.ComposeTarget != ManagedComposeTarget {
		return releaseManifest{}, errors.New("release manifest references an unsupported Compose target")
	}
	if manifest.VersionTarget != "" && manifest.VersionTarget != "releases/"+manifest.Version+"/version.json" {
		return releaseManifest{}, errors.New("release manifest references an unsupported version target")
	}

	return manifest, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("release manifest contains trailing JSON")
		}
		return fmt.Errorf("decode release manifest trailer: %w", err)
	}

	return nil
}

func downloadTarget(trustedUpdater *updater.Updater, name string, maxLength int64) ([]byte, error) {
	info, err := trustedUpdater.GetTargetInfo(name)
	if err != nil {
		return nil, fmt.Errorf("resolve signed target %s: %w", name, err)
	}
	if info.Length < 1 || info.Length > maxLength {
		return nil, fmt.Errorf("signed target %s exceeds its allowed size", name)
	}
	_, contents, err := trustedUpdater.DownloadTarget(info, "", "")
	if err != nil {
		return nil, fmt.Errorf("download signed target %s: %w", name, err)
	}

	return contents, nil
}
