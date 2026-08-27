package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"gopkg.in/yaml.v3"
)

var (
	instanceIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	controlTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	versionPattern      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Message string `json:"message"`
}

type Report struct {
	SchemaVersion int              `json:"schema_version"`
	Status        Status           `json:"status"`
	Instance      *instance.Config `json:"instance,omitempty"`
	Checks        []Check          `json:"checks"`
}

type Probe interface {
	OperatingSystem() string
	CommandOutput(context.Context, string, ...string) (string, error)
}

type Service struct {
	StateDir string
	Probe    Probe
}

func (service Service) Run(ctx context.Context, instanceID string) Report {
	report := Report{SchemaVersion: 1, Status: StatusPass, Checks: make([]Check, 0, 8)}
	if !instanceIDPattern.MatchString(instanceID) {
		report.add("instance-id", StatusFail, "Managed instance identifier is invalid")
		return report
	}
	probe := service.Probe
	if probe == nil {
		probe = RealProbe{}
	}
	diagnosticContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	if probe.OperatingSystem() == "linux" {
		report.add("platform", StatusPass, "Linux platform detected")
	} else {
		report.add("platform", StatusFail, "GEOFlow Updater requires Linux")
	}
	if _, err := probe.CommandOutput(diagnosticContext, "docker", "compose", "version"); err != nil {
		report.add("docker-compose", StatusFail, "Docker Compose v2 is unavailable: "+err.Error())
	} else {
		report.add("docker-compose", StatusPass, "Docker Compose v2 is available")
	}

	stateDir := service.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/geoflow-updater"
	}
	instanceDir := filepath.Join(stateDir, "instances", instanceID)
	instanceInfo, err := os.Lstat(instanceDir)
	if err != nil || !instanceInfo.IsDir() || instanceInfo.Mode()&os.ModeSymlink != 0 {
		report.add("instance-config", StatusFail, "Managed instance state directory is unavailable")
		return report
	}
	configPath := filepath.Join(instanceDir, "instance.yml")
	config, err := loadConfig(configPath)
	if err != nil {
		report.add("instance-config", StatusFail, "Managed instance configuration is unavailable: "+err.Error())
		return report
	}
	if config.ID != instanceID {
		report.add("instance-config", StatusFail, "Managed instance identifier does not match its directory")
		return report
	}
	report.Instance = &config
	report.add("instance-config", StatusPass, "Managed instance configuration is valid")

	if err := validateOwnedPath(instanceDir, config.ComposeFile); err != nil {
		report.add("managed-compose", StatusFail, err.Error())
	} else if err := regularFile(config.ComposeFile); err != nil {
		report.add("managed-compose", StatusFail, "Managed Compose file is unavailable: "+err.Error())
	} else {
		report.add("managed-compose", StatusPass, "Managed Compose file is present")
	}

	if err := validateInstanceRoot(config.Root); err != nil {
		report.add("instance-root", StatusFail, err.Error())
	} else {
		report.add("instance-root", StatusPass, "Instance root and persistent storage are present")
	}
	if err := validateInstalledVersion(config); err != nil {
		report.add("installed-version", StatusFail, err.Error())
	} else {
		report.add("installed-version", StatusPass, "Installed GEOFlow version matches the enrolled release")
	}
	if err := validatePostgresCluster(config); err != nil {
		report.add("postgres-data", StatusFail, err.Error())
	} else {
		report.add("postgres-data", StatusPass, "Existing PostgreSQL data directory and major version are verified")
	}

	if err := validateOwnedPath(instanceDir, config.ControlToken); err != nil {
		report.add("control-token", StatusFail, err.Error())
	} else if err := privateToken(config.ControlToken); err != nil {
		report.add("control-token", StatusFail, "Control token is invalid: "+err.Error())
	} else {
		report.add("control-token", StatusPass, "Control token permissions are restricted")
	}

	if err := validateOwnedPath(instanceDir, config.EnvironmentFile); err != nil {
		report.add("release-pins", StatusFail, err.Error())
	} else if err := validateReleasePins(config, config.EnvironmentFile, stateDir); err != nil {
		report.add("release-pins", StatusFail, "Release image pins are invalid: "+err.Error())
	} else {
		report.add("release-pins", StatusPass, "Application and web images are pinned by digest")
	}

	if err := validateManagedDeployment(diagnosticContext, probe, stateDir, instanceDir, config); err != nil {
		report.add("managed-deployment", StatusFail, "Managed deployment is unavailable: "+err.Error())
	} else {
		report.add("managed-deployment", StatusPass, "Managed Compose configuration and required services are running")
	}

	return report
}

func (report *Report) add(id string, status Status, message string) {
	report.Checks = append(report.Checks, Check{ID: id, Status: status, Message: message})
	if status == StatusFail || (status == StatusWarn && report.Status == StatusPass) {
		report.Status = status
	}
}

func loadConfig(path string) (instance.Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return instance.Config{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return instance.Config{}, errors.New("instance configuration is not a bounded regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return instance.Config{}, err
	}
	var config instance.Config
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return instance.Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return instance.Config{}, errors.New("instance configuration contains multiple YAML documents")
	}
	if config.SchemaVersion != 1 || config.ID == "" || config.Root == "" || config.ReleaseSequence == 0 || !versionPattern.MatchString(config.Version) ||
		(config.PostgresMajor != "16" && config.PostgresMajor != "18") ||
		(config.RedisMajor != "7" && config.RedisMajor != "8") ||
		!filepath.IsAbs(config.PostgresDataDir) || !filepath.IsAbs(config.PostgresMount) {
		return instance.Config{}, errors.New("required instance fields are missing")
	}

	return config, nil
}

func validateOwnedPath(instanceDir string, path string) error {
	relative, err := filepath.Rel(instanceDir, filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("managed file path escapes the instance state directory")
	}

	return nil
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}

	return nil
}

func validateInstanceRoot(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("instance root is not absolute")
	}
	if err := regularFile(filepath.Join(root, ".env.prod")); err != nil {
		return errors.New("instance .env.prod is unavailable")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("instance root is unavailable or symbolic")
	}
	info, err := os.Lstat(filepath.Join(root, "storage"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("instance storage directory is unavailable")
	}

	return nil
}

func validateInstalledVersion(config instance.Config) error {
	path := filepath.Join(config.Root, "version.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return errors.New("installed version.json is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return errors.New("installed version.json is unavailable")
	}
	var document struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(contents, &document) != nil || document.Version != config.Version {
		return errors.New("installed GEOFlow version no longer matches the enrolled release")
	}

	return nil
}

func validatePostgresCluster(config instance.Config) error {
	var versionPath string
	switch {
	case config.PostgresMajor == "16" && config.PostgresMount == "/var/lib/postgresql/data":
		versionPath = filepath.Join(config.PostgresDataDir, "PG_VERSION")
	case config.PostgresMajor == "16" && config.PostgresMount == "/var/lib/postgresql":
		versionPath = filepath.Join(config.PostgresDataDir, "data", "PG_VERSION")
	case config.PostgresMajor == "18" && config.PostgresMount == "/var/lib/postgresql":
		versionPath = filepath.Join(config.PostgresDataDir, "18", "docker", "PG_VERSION")
	default:
		return errors.New("PostgreSQL data mount is unsupported")
	}
	contents, err := os.ReadFile(versionPath)
	if err != nil || strings.TrimSpace(string(contents)) != config.PostgresMajor {
		return errors.New("PostgreSQL data directory no longer matches the enrolled major version")
	}

	return nil
}

func privateToken(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	if info.Mode().Perm()&0o007 != 0 || info.Mode().Perm()&0o020 != 0 {
		return fmt.Errorf("permissions %o allow unintended writes or public access", info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !controlTokenPattern.MatchString(strings.TrimSpace(string(contents))) {
		return errors.New("token format is invalid")
	}

	return nil
}

func validateReleasePins(config instance.Config, path string, stateDir string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return errors.New("release environment is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	values := make(map[string]string)
	allowed := map[string]struct{}{
		"GEOFLOW_COMPOSE_PROJECT_NAME":        {},
		"GEOFLOW_INSTANCE_ID":                 {},
		"GEOFLOW_INSTANCE_ROOT":               {},
		"GEOFLOW_RELEASE_SEQUENCE":            {},
		"GEOFLOW_VERSION":                     {},
		"GEOFLOW_UPDATER_STATE_DIR":           {},
		"GEOFLOW_UPDATER_GROUP_ID":            {},
		"GEOFLOW_APP_IMAGE":                   {},
		"GEOFLOW_WEB_IMAGE":                   {},
		"GEOFLOW_POSTGRES_IMAGE":              {},
		"GEOFLOW_POSTGRES_DATA_DIR":           {},
		"GEOFLOW_POSTGRES_CONTAINER_DATA_DIR": {},
		"GEOFLOW_REDIS_IMAGE":                 {},
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return errors.New("release environment contains a malformed line")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("release environment contains unsupported key %q", key)
		}
		if _, exists := values[key]; exists {
			return fmt.Errorf("release environment contains duplicate key %q", key)
		}
		values[key] = decodeEnvironmentValue(value)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(values) != len(allowed) {
		return errors.New("release environment is missing required keys")
	}
	groupID, err := strconv.Atoi(values["GEOFLOW_UPDATER_GROUP_ID"])
	if err != nil || groupID < 1 {
		return errors.New("release environment contains an invalid updater group id")
	}
	if values["GEOFLOW_COMPOSE_PROJECT_NAME"] != "geoflow-laravel-prod" ||
		values["GEOFLOW_INSTANCE_ID"] != config.ID ||
		values["GEOFLOW_INSTANCE_ROOT"] != config.Root ||
		values["GEOFLOW_RELEASE_SEQUENCE"] != strconv.FormatUint(config.ReleaseSequence, 10) ||
		values["GEOFLOW_VERSION"] != config.Version ||
		values["GEOFLOW_UPDATER_STATE_DIR"] != stateDir ||
		values["GEOFLOW_POSTGRES_DATA_DIR"] != config.PostgresDataDir ||
		values["GEOFLOW_POSTGRES_CONTAINER_DATA_DIR"] != config.PostgresMount {
		return errors.New("release environment does not match the enrolled instance")
	}

	return managed.ValidateSelectedImages(
		values["GEOFLOW_APP_IMAGE"],
		values["GEOFLOW_WEB_IMAGE"],
		values["GEOFLOW_POSTGRES_IMAGE"],
		values["GEOFLOW_REDIS_IMAGE"],
	)
}

func decodeEnvironmentValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		decoded, err := strconv.Unquote(value)
		if err == nil {
			return decoded
		}
	}

	return value
}

func validateManagedDeployment(ctx context.Context, probe Probe, stateDir string, instanceDir string, config instance.Config) error {
	for _, path := range []string{config.ComposeFile, config.EnvironmentFile} {
		if err := validateOwnedPath(instanceDir, path); err != nil {
			return err
		}
		if err := regularFile(path); err != nil {
			return err
		}
	}
	if err := validateInstanceRoot(config.Root); err != nil {
		return err
	}
	if err := validateInstalledVersion(config); err != nil {
		return err
	}
	if err := validatePostgresCluster(config); err != nil {
		return err
	}
	if err := validateReleasePins(config, config.EnvironmentFile, stateDir); err != nil {
		return err
	}

	composeArguments := []string{
		"compose",
		"--env-file", filepath.Join(config.Root, ".env.prod"),
		"--env-file", config.EnvironmentFile,
		"-f", config.ComposeFile,
	}
	if _, err := probe.CommandOutput(ctx, "docker", append(composeArguments, "config", "--quiet")...); err != nil {
		return fmt.Errorf("Compose configuration check failed: %w", err)
	}

	containers := map[string]string{
		"geoflow-postgres-prod":            "healthy",
		"geoflow-redis-prod":               "healthy",
		"geoflow-app-prod":                 "healthy",
		"geoflow-web-prod":                 "healthy",
		"geoflow-queue-prod":               "none",
		"geoflow-knowledge-queue-prod":     "none",
		"geoflow-system-update-queue-prod": "none",
		"geoflow-scheduler-prod":           "none",
		"geoflow-reverb-prod":              "none",
	}
	arguments := []string{"inspect", "--format={{.Name}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"}
	for name := range containers {
		arguments = append(arguments, name)
	}
	output, err := probe.CommandOutput(ctx, "docker", arguments...)
	if err != nil {
		return fmt.Errorf("inspect required containers: %w", err)
	}
	seen := make(map[string]struct{}, len(containers))
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			continue
		}
		name := strings.TrimPrefix(parts[0], "/")
		expectedHealth, required := containers[name]
		if !required {
			continue
		}
		if parts[1] != "running" || parts[2] != expectedHealth {
			return fmt.Errorf("container %s is %s with health %s", name, parts[1], parts[2])
		}
		seen[name] = struct{}{}
	}
	if len(seen) != len(containers) {
		return fmt.Errorf("only %d of %d required containers are running", len(seen), len(containers))
	}

	return nil
}

type RealProbe struct{}

func (RealProbe) OperatingSystem() string {
	return runtime.GOOS
}

func (RealProbe) CommandOutput(ctx context.Context, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.Output()
	if len(output) > 64*1024 {
		return "", errors.New("command output exceeded the diagnostic limit")
	}

	return string(output), err
}
