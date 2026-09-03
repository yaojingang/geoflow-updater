package enrollment

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"gopkg.in/yaml.v3"
)

var (
	instanceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	versionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

type ReleaseResolver interface {
	Current(context.Context) (managed.Release, error)
}

type Service struct {
	StateDir       string
	Now            func() time.Time
	Random         io.Reader
	Releases       ReleaseResolver
	ControlGroupID func() (int, error)
	Chown          func(string, int, int) error
	RootAccess     func(string) error
}

type Request struct {
	InstanceID string
	Root       string
}

type Result struct {
	Instance     instance.Config
	ControlToken string
}

func (service Service) Enroll(ctx context.Context, request Request) (Result, error) {
	if !instanceIDPattern.MatchString(request.InstanceID) {
		return Result{}, errors.New("instance id must start with a lowercase letter and contain only lowercase letters, numbers, or hyphens")
	}
	if request.InstanceID != "primary" {
		return Result{}, errors.New("single-host enrollment supports only the primary instance")
	}

	rootAccess := service.RootAccess
	if rootAccess == nil {
		rootAccess = validateServiceSandboxRoot
	}
	if filepath.IsAbs(request.Root) {
		if err := rootAccess(filepath.Clean(request.Root)); err != nil {
			return Result{}, err
		}
	}
	root, err := canonicalRoot(request.Root)
	if err != nil {
		return Result{}, err
	}
	if err := rootAccess(root); err != nil {
		return Result{}, err
	}
	if err := validateLayout(root); err != nil {
		return Result{}, err
	}
	installedVersion, err := readInstalledVersion(root)
	if err != nil {
		return Result{}, err
	}
	if service.Releases == nil {
		return Result{}, errors.New("trusted release resolver is required")
	}

	release, err := service.Releases.Current(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("resolve trusted release: %w", err)
	}
	if err := release.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate trusted release: %w", err)
	}
	if installedVersion != release.Version {
		return Result{}, fmt.Errorf("installed GEOFlow %s must match signed bridge release %s before enrollment", installedVersion, release.Version)
	}
	infrastructure, err := installedInfrastructure(filepath.Join(root, ".env.prod"), root)
	if err != nil {
		return Result{}, err
	}
	postgresImage, redisImage, err := release.InfrastructureImages(infrastructure.PostgresMajor, infrastructure.RedisMajor)
	if err != nil {
		return Result{}, err
	}
	controlGroupID, err := service.controlGroupID()
	if err != nil {
		return Result{}, fmt.Errorf("resolve updater control group: %w", err)
	}

	stateDir := service.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/geoflow-updater"
	}
	instanceDir := filepath.Join(stateDir, "instances", request.InstanceID)
	if _, err := os.Stat(instanceDir); err == nil {
		return Result{}, fmt.Errorf("instance %q is already enrolled", request.InstanceID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("inspect instance state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(instanceDir), 0o750); err != nil {
		return Result{}, fmt.Errorf("create instances state: %w", err)
	}
	if err := os.Mkdir(instanceDir, 0o750); err != nil {
		return Result{}, fmt.Errorf("create instance state: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(instanceDir)
		}
	}()

	config := instance.Config{
		SchemaVersion:   1,
		ID:              request.InstanceID,
		Root:            root,
		ComposeFile:     filepath.Join(instanceDir, "docker-compose.managed.yml"),
		EnvironmentFile: filepath.Join(instanceDir, "release.env"),
		ControlToken:    filepath.Join(instanceDir, "control.token"),
		ReleaseSequence: release.Sequence,
		Version:         release.Version,
		PostgresMajor:   infrastructure.PostgresMajor,
		PostgresDataDir: infrastructure.PostgresDataDir,
		PostgresMount:   infrastructure.PostgresContainerDataDir,
		RedisMajor:      infrastructure.RedisMajor,
		EnrolledAt:      service.now().UTC(),
	}

	token, err := service.controlToken()
	if err != nil {
		return Result{}, fmt.Errorf("generate control token: %w", err)
	}
	instanceYAML, err := yaml.Marshal(config)
	if err != nil {
		return Result{}, fmt.Errorf("encode instance config: %w", err)
	}

	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{config.ControlToken, []byte(token + "\n"), 0o640},
		{config.ComposeFile, release.ComposeTemplate, 0o640},
		{config.EnvironmentFile, releaseEnvironment(config, release, stateDir, controlGroupID, postgresImage, redisImage, infrastructure), 0o640},
		{filepath.Join(instanceDir, "instance.yml"), instanceYAML, 0o640},
	}
	for _, file := range files {
		if err := writeExclusive(file.path, file.data, file.mode); err != nil {
			return Result{}, err
		}
	}
	chown := service.Chown
	if chown == nil {
		chown = os.Chown
	}
	if err := chown(config.ControlToken, -1, controlGroupID); err != nil {
		return Result{}, fmt.Errorf("set control token group: %w", err)
	}
	committed = true

	return Result{Instance: config}, nil
}

func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("instance root must be an absolute path")
	}
	cleaned := filepath.Clean(root)
	info, err := os.Lstat(cleaned)
	if err != nil {
		return "", fmt.Errorf("inspect instance root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("instance root must not be a symbolic link")
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve instance root: %w", err)
	}

	return resolved, nil
}

func validateServiceSandboxRoot(root string) error {
	for _, protected := range []string{"/boot", "/efi", "/etc", "/home", "/root", "/run", "/tmp", "/usr", "/var/tmp"} {
		if root == protected || strings.HasPrefix(root, protected+string(filepath.Separator)) {
			return errors.New("instance root is blocked by the installed systemd sandbox")
		}
	}

	return nil
}

func validateLayout(root string) error {
	environmentPath := filepath.Join(root, ".env.prod")
	if info, err := os.Lstat(environmentPath); err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return errors.New("instance root must contain a bounded regular .env.prod file")
	}
	storagePath := filepath.Join(root, "storage")
	if info, err := os.Lstat(storagePath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("instance root must contain a non-symbolic storage directory")
	}

	return nil
}

func readInstalledVersion(root string) (string, error) {
	path := filepath.Join(root, "version.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return "", errors.New("instance root must contain a bounded regular version.json file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("instance root must contain a readable version.json file")
	}
	var document struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(contents, &document); err != nil || !versionPattern.MatchString(document.Version) {
		return "", errors.New("instance version.json does not contain a valid semantic version")
	}

	return document.Version, nil
}

func releaseEnvironment(config instance.Config, release managed.Release, stateDir string, controlGroupID int, postgresImage string, redisImage string, infrastructure infrastructureConfig) []byte {
	return []byte(
		"GEOFLOW_COMPOSE_PROJECT_NAME=geoflow-laravel-prod\n" +
			"GEOFLOW_INSTANCE_ID=" + config.ID + "\n" +
			"GEOFLOW_INSTANCE_ROOT=" + strconv.Quote(config.Root) + "\n" +
			"GEOFLOW_RELEASE_SEQUENCE=" + strconv.FormatUint(release.Sequence, 10) + "\n" +
			"GEOFLOW_VERSION=" + release.Version + "\n" +
			"GEOFLOW_UPDATER_STATE_DIR=" + strconv.Quote(stateDir) + "\n" +
			"GEOFLOW_UPDATER_GROUP_ID=" + strconv.Itoa(controlGroupID) + "\n" +
			"GEOFLOW_APP_IMAGE=" + release.AppImage + "\n" +
			"GEOFLOW_WEB_IMAGE=" + release.WebImage + "\n" +
			"GEOFLOW_POSTGRES_IMAGE=" + postgresImage + "\n" +
			"GEOFLOW_POSTGRES_DATA_DIR=" + strconv.Quote(infrastructure.PostgresDataDir) + "\n" +
			"GEOFLOW_POSTGRES_CONTAINER_DATA_DIR=" + infrastructure.PostgresContainerDataDir + "\n" +
			"GEOFLOW_REDIS_IMAGE=" + redisImage + "\n",
	)
}

type infrastructureConfig struct {
	PostgresMajor            string
	RedisMajor               string
	PostgresDataDir          string
	PostgresContainerDataDir string
}

func installedInfrastructure(environmentPath string, root string) (infrastructureConfig, error) {
	contents, err := os.ReadFile(environmentPath)
	if err != nil {
		return infrastructureConfig{}, fmt.Errorf("read instance .env.prod: %w", err)
	}
	if len(contents) > 1024*1024 {
		return infrastructureConfig{}, errors.New("instance .env.prod exceeds the 1 MiB safety limit")
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "APP_KEY" && key != "DB_CONNECTION" && key != "DB_HOST" && key != "DB_PASSWORD" && key != "REDIS_HOST" && key != "PGVECTOR_IMAGE" && key != "REDIS_IMAGE" && key != "GEOFLOW_UPDATER_POSTGRES_MAJOR" && key != "GEOFLOW_UPDATER_REDIS_MAJOR" && key != "POSTGRES_DATA_DIR" && key != "POSTGRES_CONTAINER_DATA_DIR" {
			continue
		}
		values[key] = unquoteEnvironmentValue(value)
	}
	encodedKey := strings.TrimPrefix(values["APP_KEY"], "base64:")
	decodedKey, keyError := base64.StdEncoding.DecodeString(encodedKey)
	if values["APP_KEY"] == encodedKey || keyError != nil || len(decodedKey) != 32 {
		return infrastructureConfig{}, errors.New("instance APP_KEY must contain a valid 32-byte base64 key before enrollment")
	}
	if values["DB_CONNECTION"] != "pgsql" || values["DB_HOST"] != "postgres" || values["DB_PASSWORD"] == "" {
		return infrastructureConfig{}, errors.New("managed enrollment requires the bundled PostgreSQL connection and a non-empty DB_PASSWORD")
	}
	if values["REDIS_HOST"] != "redis" {
		return infrastructureConfig{}, errors.New("managed enrollment requires the bundled Redis service")
	}

	postgresMajor, err := resolveMajor(values["GEOFLOW_UPDATER_POSTGRES_MAJOR"], values["PGVECTOR_IMAGE"], "PostgreSQL", "GEOFLOW_UPDATER_POSTGRES_MAJOR", func(tag string) (string, bool) {
		matches := regexp.MustCompile(`^pg(16|18)(?:[-.].*)?$`).FindStringSubmatch(tag)
		if len(matches) != 2 {
			return "", false
		}
		return matches[1], true
	})
	if err != nil {
		return infrastructureConfig{}, err
	}
	redisMajor, err := resolveMajor(values["GEOFLOW_UPDATER_REDIS_MAJOR"], values["REDIS_IMAGE"], "Redis", "GEOFLOW_UPDATER_REDIS_MAJOR", func(tag string) (string, bool) {
		matches := regexp.MustCompile(`^(7|8)(?:[-.].*)?$`).FindStringSubmatch(tag)
		if len(matches) != 2 {
			return "", false
		}
		return matches[1], true
	})
	if err != nil {
		return infrastructureConfig{}, err
	}
	postgresDataDir := values["POSTGRES_DATA_DIR"]
	if postgresDataDir == "" {
		postgresDataDir = filepath.Join(root, "docker-data", "prod", "postgres")
	} else if !filepath.IsAbs(postgresDataDir) {
		postgresDataDir = filepath.Join(root, postgresDataDir)
	}
	postgresDataDir, err = filepath.Abs(filepath.Clean(postgresDataDir))
	if err != nil {
		return infrastructureConfig{}, fmt.Errorf("resolve PostgreSQL data directory: %w", err)
	}
	resolvedDataDir, err := filepath.EvalSymlinks(postgresDataDir)
	if err != nil {
		return infrastructureConfig{}, errors.New("existing PostgreSQL data directory is unavailable")
	}
	dataInfo, err := os.Stat(resolvedDataDir)
	if err != nil || !dataInfo.IsDir() {
		return infrastructureConfig{}, errors.New("existing PostgreSQL data directory is unavailable")
	}
	containerDataDir := values["POSTGRES_CONTAINER_DATA_DIR"]
	if containerDataDir == "" {
		if postgresMajor == "16" {
			containerDataDir = "/var/lib/postgresql/data"
		} else {
			containerDataDir = "/var/lib/postgresql"
		}
	}
	var versionPath string
	switch {
	case postgresMajor == "16" && containerDataDir == "/var/lib/postgresql/data":
		versionPath = filepath.Join(resolvedDataDir, "PG_VERSION")
	case postgresMajor == "16" && containerDataDir == "/var/lib/postgresql":
		versionPath = filepath.Join(resolvedDataDir, "data", "PG_VERSION")
	case postgresMajor == "18" && containerDataDir == "/var/lib/postgresql":
		versionPath = filepath.Join(resolvedDataDir, "18", "docker", "PG_VERSION")
	default:
		return infrastructureConfig{}, fmt.Errorf("PostgreSQL %s data mount target %q is unsupported", postgresMajor, containerDataDir)
	}
	versionContents, err := os.ReadFile(versionPath)
	if err != nil || strings.TrimSpace(string(versionContents)) != postgresMajor {
		return infrastructureConfig{}, fmt.Errorf("PostgreSQL data directory does not contain the expected major %s cluster", postgresMajor)
	}

	return infrastructureConfig{
		PostgresMajor:            postgresMajor,
		RedisMajor:               redisMajor,
		PostgresDataDir:          resolvedDataDir,
		PostgresContainerDataDir: containerDataDir,
	}, nil
}

func resolveMajor(explicit string, image string, label string, overrideName string, fromTag func(string) (string, bool)) (string, error) {
	if explicit != "" {
		if (label == "PostgreSQL" && (explicit == "16" || explicit == "18")) || (label == "Redis" && (explicit == "7" || explicit == "8")) {
			return explicit, nil
		}
		return "", fmt.Errorf("%s major override is unsupported", label)
	}
	if image == "" {
		return "", fmt.Errorf("%s major cannot be inferred because its image setting is empty; set %s explicitly", label, overrideName)
	}
	tag, ok := imageTag(image)
	if !ok {
		return "", fmt.Errorf("%s major cannot be inferred from %q; set %s explicitly", label, image, overrideName)
	}
	major, ok := fromTag(tag)
	if !ok {
		return "", fmt.Errorf("%s major in %q is unsupported", label, image)
	}

	return major, nil
}

func imageTag(image string) (string, bool) {
	withoutDigest, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	separator := strings.LastIndex(withoutDigest, ":")
	if separator < 0 || separator < strings.LastIndex(withoutDigest, "/") {
		return "", false
	}

	return withoutDigest[separator+1:], true
}

func unquoteEnvironmentValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}

	return value
}

func (service Service) controlGroupID() (int, error) {
	if service.ControlGroupID != nil {
		return service.ControlGroupID()
	}

	return os.Getgid(), nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set permissions on %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}

func (service Service) now() time.Time {
	if service.Now != nil {
		return service.Now()
	}

	return time.Now()
}

func (service Service) controlToken() (string, error) {
	random := service.Random
	if random == nil {
		random = rand.Reader
	}
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
