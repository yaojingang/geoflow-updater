package managed

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

var (
	appImagePattern      = regexp.MustCompile(`^ghcr\.io/yaojingang/geoflow-app@sha256:[a-f0-9]{64}$`)
	webImagePattern      = regexp.MustCompile(`^ghcr\.io/yaojingang/geoflow-web@sha256:[a-f0-9]{64}$`)
	postgresImagePattern = regexp.MustCompile(`^pgvector/pgvector@sha256:[a-f0-9]{64}$`)
	redisImagePattern    = regexp.MustCompile(`^redis@sha256:[a-f0-9]{64}$`)
	versionPattern       = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

const UpdaterProtocolVersion uint64 = 2

type Release struct {
	Sequence               uint64
	MinimumUpdaterProtocol uint64
	Version                string
	SourceCommit           string
	AppImage               string
	WebImage               string
	PostgresImages         map[string]string
	RedisImages            map[string]string
	ComposeTemplate        []byte
	VersionDocument        []byte
}

func (release Release) Validate() error {
	if release.Sequence == 0 {
		return errors.New("release sequence must be positive")
	}
	if release.MinimumUpdaterProtocol > UpdaterProtocolVersion {
		return errors.New("release requires a newer updater protocol")
	}
	if !versionPattern.MatchString(release.Version) {
		return errors.New("release version must be semantic")
	}
	if !appImagePattern.MatchString(release.AppImage) {
		return errors.New("app image must use the official registry and a sha256 digest")
	}
	if !webImagePattern.MatchString(release.WebImage) {
		return errors.New("web image must use the official registry and a sha256 digest")
	}
	if err := validateImageSet("PostgreSQL", release.PostgresImages, []string{"16", "18"}, postgresImagePattern); err != nil {
		return err
	}
	if err := validateImageSet("Redis", release.RedisImages, []string{"7", "8"}, redisImagePattern); err != nil {
		return err
	}
	if len(release.ComposeTemplate) == 0 {
		return errors.New("managed Compose template is required")
	}
	if len(release.VersionDocument) > 1024*1024 {
		return errors.New("signed version document exceeds the size limit")
	}
	if len(release.VersionDocument) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(release.VersionDocument))
		var document struct {
			Version string `json:"version"`
		}
		if err := decoder.Decode(&document); err != nil || document.Version != release.Version {
			return errors.New("signed version document does not match the release")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("signed version document contains trailing JSON")
		}
	}

	return nil
}

func (release Release) InfrastructureImages(postgresMajor string, redisMajor string) (string, string, error) {
	postgresImage, ok := release.PostgresImages[postgresMajor]
	if !ok {
		return "", "", errors.New("signed release does not support the installed PostgreSQL major")
	}
	redisImage, ok := release.RedisImages[redisMajor]
	if !ok {
		return "", "", errors.New("signed release does not support the installed Redis major")
	}
	if !postgresImagePattern.MatchString(postgresImage) || !redisImagePattern.MatchString(redisImage) {
		return "", "", errors.New("signed infrastructure image selection is invalid")
	}

	return postgresImage, redisImage, nil
}

func ValidateSelectedImages(appImage string, webImage string, postgresImage string, redisImage string) error {
	if !appImagePattern.MatchString(appImage) {
		return errors.New("app image is not an official digest pin")
	}
	if !webImagePattern.MatchString(webImage) {
		return errors.New("web image is not an official digest pin")
	}
	if !postgresImagePattern.MatchString(postgresImage) {
		return errors.New("PostgreSQL image is not an official digest pin")
	}
	if !redisImagePattern.MatchString(redisImage) {
		return errors.New("Redis image is not an official digest pin")
	}

	return nil
}

func validateImageSet(label string, images map[string]string, expected []string, pattern *regexp.Regexp) error {
	if len(images) != len(expected) {
		return errors.New(label + " signed image set is incomplete")
	}
	for _, major := range expected {
		image, ok := images[major]
		if !ok || !pattern.MatchString(image) {
			return errors.New(label + " signed image for major " + major + " is invalid")
		}
	}

	return nil
}
