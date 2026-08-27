package deployment

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"gopkg.in/yaml.v3"
)

var instanceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type ReleaseResolver interface {
	Current(context.Context) (managed.Release, error)
}

type Diagnostician interface {
	Run(context.Context, string) doctor.Report
}

type CommandRunner interface {
	Run(context.Context, io.Reader, io.Writer, string, ...string) error
}

type RecoveryStore interface {
	Create(context.Context, instance.Config, string, recovery.Database) (recovery.Point, error)
	Restore(context.Context, instance.Config, string, recovery.Database) error
	List(string) ([]recovery.Point, error)
}

type Service struct {
	StateDir    string
	Releases    ReleaseResolver
	Doctor      Diagnostician
	Runner      CommandRunner
	Recoveries  RecoveryStore
	candidateMu sync.Mutex
}

func (service *Service) Resolve(ctx context.Context, instanceID string) (managed.Release, error) {
	if service.Releases == nil {
		return managed.Release{}, errors.New("trusted release resolver is unavailable")
	}
	release, err := service.Releases.Current(ctx)
	if err != nil {
		return managed.Release{}, fmt.Errorf("resolve signed release: %w", err)
	}
	if err := release.Validate(); err != nil {
		return managed.Release{}, fmt.Errorf("validate signed release: %w", err)
	}
	return release, nil
}

func (service *Service) Preflight(ctx context.Context, instanceID string, release managed.Release) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	if release.Sequence <= config.ReleaseSequence {
		return fmt.Errorf("signed release sequence %d does not exceed installed sequence %d", release.Sequence, config.ReleaseSequence)
	}
	if len(release.VersionDocument) == 0 {
		return errors.New("signed release is missing its version document")
	}
	if _, _, err := release.InfrastructureImages(config.PostgresMajor, config.RedisMajor); err != nil {
		return err
	}
	if service.Doctor == nil {
		return errors.New("deployment diagnostics are unavailable")
	}
	report := service.Doctor.Run(ctx, instanceID)
	if report.Status != doctor.StatusPass {
		return fmt.Errorf("current deployment diagnostics returned %s", report.Status)
	}
	return nil
}

func (service *Service) Pull(ctx context.Context, instanceID string, release managed.Release) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	composePath, environmentPath, err := service.writeCandidate(config, release)
	if err != nil {
		return err
	}
	arguments := composeArguments(config.Root, environmentPath, composePath)
	arguments = append(arguments, "pull")
	return service.runner().Run(ctx, nil, io.Discard, "docker", arguments...)
}

func (service *Service) Quiesce(ctx context.Context, instanceID string) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	arguments := composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile)
	_ = service.runner().Run(ctx, nil, io.Discard, "docker", append(arguments, "exec", "-T", "app", "php", "artisan", "down", "--retry=60")...)
	services := []string{"queue", "knowledge-queue", "system-update-queue", "scheduler", "reverb", "web", "app"}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append(append(arguments, "stop"), services...)...); err != nil {
		resumeErr := service.Resume(ctx, instanceID)
		return errors.Join(fmt.Errorf("stop application services: %w", err), resumeErr)
	}
	return nil
}

func (service *Service) CreateRecoveryPoint(ctx context.Context, instanceID string, reason string) (string, error) {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return "", err
	}
	if service.Recoveries == nil {
		return "", errors.New("recovery point store is unavailable")
	}
	point, err := service.Recoveries.Create(ctx, config, reason, postgresDatabase{config: config, runner: service.runner()})
	if err != nil {
		return "", err
	}
	return point.ID, nil
}

func (service *Service) Migrate(ctx context.Context, instanceID string, _ managed.Release) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	composePath, environmentPath := service.candidatePaths(config)
	arguments := composeArguments(config.Root, environmentPath, composePath)
	arguments = append(arguments, "run", "--rm", "--no-deps", "init", "php", "artisan", "migrate", "--force")
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", arguments...); err != nil {
		return fmt.Errorf("run database migrations: %w", err)
	}
	return nil
}

func (service *Service) Activate(_ context.Context, instanceID string, release managed.Release) error {
	service.candidateMu.Lock()
	defer service.candidateMu.Unlock()
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	composePath, environmentPath := service.candidatePaths(config)
	for _, path := range []string{composePath, environmentPath} {
		if err := regularFile(path); err != nil {
			return errors.New("prepared signed deployment is unavailable")
		}
	}
	if err := replaceFromFile(composePath, config.ComposeFile, 0o640); err != nil {
		return fmt.Errorf("activate managed Compose file: %w", err)
	}
	if err := replaceFromFile(environmentPath, config.EnvironmentFile, 0o640); err != nil {
		return fmt.Errorf("activate release environment: %w", err)
	}
	config.ReleaseSequence = release.Sequence
	config.Version = release.Version
	encodedConfig, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := replaceContents(filepath.Join(filepath.Dir(config.ComposeFile), "instance.yml"), encodedConfig, 0o640); err != nil {
		return fmt.Errorf("activate instance configuration: %w", err)
	}
	if err := replaceContents(filepath.Join(config.Root, "version.json"), release.VersionDocument, 0o640); err != nil {
		return fmt.Errorf("activate installed version marker: %w", err)
	}
	return nil
}

func (service *Service) Rollback(ctx context.Context, instanceID string, recoveryPointID string) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	if service.Recoveries == nil {
		return errors.New("recovery point store is unavailable")
	}
	return service.Recoveries.Restore(ctx, config, recoveryPointID, postgresDatabase{config: config, runner: service.runner()})
}

func (service *Service) Resume(ctx context.Context, instanceID string) error {
	config, err := service.loadConfig(instanceID)
	if err != nil {
		return err
	}
	arguments := composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile)
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append(arguments, "run", "--rm", "--no-deps", "init", "php", "artisan", "up")...); err != nil {
		return fmt.Errorf("disable maintenance mode: %w", err)
	}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append(arguments, "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", "180")...); err != nil {
		return fmt.Errorf("start managed deployment: %w", err)
	}
	return nil
}

func (service *Service) Verify(ctx context.Context, instanceID string) error {
	if service.Doctor == nil {
		return errors.New("deployment diagnostics are unavailable")
	}
	report := service.Doctor.Run(ctx, instanceID)
	if report.Status != doctor.StatusPass {
		messages := make([]string, 0)
		for _, check := range report.Checks {
			if check.Status == doctor.StatusFail {
				messages = append(messages, check.ID+": "+check.Message)
			}
		}
		return fmt.Errorf("deployment verification returned %s: %s", report.Status, strings.Join(messages, "; "))
	}
	return nil
}

func (service *Service) ListRecoveryPoints(instanceID string) ([]recovery.Point, error) {
	if service.Recoveries == nil {
		return nil, errors.New("recovery point store is unavailable")
	}
	return service.Recoveries.List(instanceID)
}

func (service *Service) writeCandidate(config instance.Config, release managed.Release) (string, string, error) {
	service.candidateMu.Lock()
	defer service.candidateMu.Unlock()
	postgresImage, redisImage, err := release.InfrastructureImages(config.PostgresMajor, config.RedisMajor)
	if err != nil {
		return "", "", err
	}
	contents, err := os.ReadFile(config.EnvironmentFile)
	if err != nil {
		return "", "", err
	}
	replacements := map[string]string{
		"GEOFLOW_RELEASE_SEQUENCE": strconv.FormatUint(release.Sequence, 10),
		"GEOFLOW_VERSION":          release.Version,
		"GEOFLOW_APP_IMAGE":        release.AppImage,
		"GEOFLOW_WEB_IMAGE":        release.WebImage,
		"GEOFLOW_POSTGRES_IMAGE":   postgresImage,
		"GEOFLOW_REDIS_IMAGE":      redisImage,
	}
	updated, err := replaceEnvironmentValues(contents, replacements)
	if err != nil {
		return "", "", err
	}
	composePath, environmentPath := service.candidatePaths(config)
	if err := os.MkdirAll(filepath.Dir(composePath), 0o750); err != nil {
		return "", "", err
	}
	if err := replaceContents(composePath, release.ComposeTemplate, 0o640); err != nil {
		return "", "", err
	}
	if err := replaceContents(environmentPath, updated, 0o640); err != nil {
		return "", "", err
	}
	return composePath, environmentPath, nil
}

func replaceEnvironmentValues(contents []byte, replacements map[string]string) ([]byte, error) {
	seen := make(map[string]struct{}, len(replacements))
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	var output strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		key, _, found := strings.Cut(line, "=")
		if value, ok := replacements[key]; ok && found {
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("release environment contains duplicate key %s", key)
			}
			seen[key] = struct{}{}
			line = key + "=" + value
		}
		output.WriteString(line)
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(seen) != len(replacements) {
		return nil, errors.New("release environment is missing updater-managed keys")
	}
	return []byte(output.String()), nil
}

func (service *Service) loadConfig(instanceID string) (instance.Config, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return instance.Config{}, errors.New("managed instance identifier is invalid")
	}
	stateDir := service.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/geoflow-updater"
	}
	instanceDir := filepath.Join(stateDir, "instances", instanceID)
	path := filepath.Join(instanceDir, "instance.yml")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1024*1024 {
		return instance.Config{}, errors.New("managed instance configuration is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return instance.Config{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var config instance.Config
	if err := decoder.Decode(&config); err != nil {
		return instance.Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return instance.Config{}, errors.New("managed instance configuration contains trailing YAML")
	}
	if config.SchemaVersion != 1 || config.ID != instanceID || config.Root == "" || config.ReleaseSequence == 0 || config.Version == "" {
		return instance.Config{}, errors.New("managed instance configuration is incomplete")
	}
	root := filepath.Clean(config.Root)
	if !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return instance.Config{}, errors.New("managed instance root is unsafe")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return instance.Config{}, errors.New("managed instance root is unavailable or unsafe")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot != root {
		return instance.Config{}, errors.New("managed instance root no longer matches its enrolled path")
	}
	for _, ownedPath := range []string{config.ComposeFile, config.EnvironmentFile, config.ControlToken} {
		relative, err := filepath.Rel(instanceDir, filepath.Clean(ownedPath))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return instance.Config{}, errors.New("managed instance path escapes its state directory")
		}
		if err := regularFile(ownedPath); err != nil {
			return instance.Config{}, errors.New("managed instance file is unavailable or unsafe")
		}
	}
	for _, siteFile := range []string{filepath.Join(root, ".env.prod"), filepath.Join(root, "version.json")} {
		if err := regularFile(siteFile); err != nil {
			return instance.Config{}, errors.New("managed site file is unavailable or unsafe")
		}
	}
	storageInfo, err := os.Lstat(filepath.Join(root, "storage"))
	if err != nil || !storageInfo.IsDir() || storageInfo.Mode()&os.ModeSymlink != 0 {
		return instance.Config{}, errors.New("managed storage directory is unavailable or unsafe")
	}
	config.Root = root
	return config, nil
}

func (service *Service) candidatePaths(config instance.Config) (string, string) {
	directory := filepath.Join(filepath.Dir(config.ComposeFile), "transaction")
	return filepath.Join(directory, "docker-compose.candidate.yml"), filepath.Join(directory, "release.candidate.env")
}

func (service *Service) runner() CommandRunner {
	if service.Runner != nil {
		return service.Runner
	}
	return RealRunner{}
}

func composeArguments(root string, environmentPath string, composePath string) []string {
	return []string{"compose", "--env-file", filepath.Join(root, ".env.prod"), "--env-file", environmentPath, "-f", composePath}
}

type postgresDatabase struct {
	config instance.Config
	runner CommandRunner
}

func (database postgresDatabase) Dump(ctx context.Context, writer io.Writer) error {
	arguments := composeArguments(database.config.Root, database.config.EnvironmentFile, database.config.ComposeFile)
	arguments = append(arguments, "exec", "-T", "postgres", "sh", "-eu", "-c", `exec pg_dump --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" --format=custom --create`)
	return database.runner.Run(ctx, nil, writer, "docker", arguments...)
}

func (database postgresDatabase) Restore(ctx context.Context, reader io.Reader) error {
	arguments := composeArguments(database.config.Root, database.config.EnvironmentFile, database.config.ComposeFile)
	if err := database.runner.Run(ctx, nil, io.Discard, "docker", append(arguments, "up", "-d", "--wait", "postgres", "redis")...); err != nil {
		return fmt.Errorf("start database services for restore: %w", err)
	}
	arguments = append(arguments, "exec", "-T", "postgres", "sh", "-eu", "-c", `exec pg_restore --exit-on-error --clean --if-exists --create --username="$POSTGRES_USER" --dbname=postgres`)
	return database.runner.Run(ctx, reader, io.Discard, "docker", arguments...)
}

type RealRunner struct{}

func (RealRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Stdin = stdin
	command.Stdout = stdout
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

type limitedBuffer struct {
	contents bytes.Buffer
}

func (buffer *limitedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	remaining := 64*1024 - buffer.contents.Len()
	if remaining > 0 {
		if len(contents) > remaining {
			contents = contents[:remaining]
		}
		_, _ = buffer.contents.Write(contents)
	}
	return written, nil
}

func (buffer *limitedBuffer) String() string { return buffer.contents.String() }

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

func replaceFromFile(source string, destination string, mode os.FileMode) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return replaceContents(destination, contents, mode)
}

func replaceContents(path string, contents []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".geoflow-updater-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
