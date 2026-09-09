package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type applicationReport struct {
	SchemaVersion     int                        `json:"schema_version"`
	Status            string                     `json:"status"`
	PlanSHA256        string                     `json:"plan_sha256"`
	Version           string                     `json:"version"`
	PendingMigrations []managed.UpgradeMigration `json:"pending_migrations"`
	EligibleOnline    bool                       `json:"eligible_online"`
}

func (service *Service) instanceDirectory(id string) string {
	root := service.StateDir
	if root == "" {
		root = "/var/lib/geoflow-updater"
	}
	return filepath.Join(root, "instances", id)
}

func (service *Service) command(ctx context.Context, config instance.Config, args ...string) error {
	return service.runner().Run(ctx, nil, io.Discard, "docker", append(composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile), args...)...)
}
func (service *Service) output(ctx context.Context, config instance.Config, args ...string) (string, error) {
	var out limitedBuffer
	err := service.runner().Run(ctx, nil, &out, "docker", append(composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile), args...)...)
	return strings.TrimSpace(out.String()), err
}
func infrastructureConfig(config instance.Config) instance.Config {
	if config.Layout == LayoutBlueGreen {
		config.ComposeFile = config.InfraComposeFile
		config.EnvironmentFile = config.InfraEnvironmentFile
	}
	return config
}

func (service *Service) prepareRelease(ctx context.Context, config instance.Config, release managed.Release, directory string, slot string) (instance.Config, error) {
	candidate := config
	candidate.ReleaseSequence = release.Sequence
	candidate.Version = release.Version
	candidate.ComposeFile = filepath.Join(directory, "docker-compose.yml")
	candidate.EnvironmentFile = filepath.Join(directory, "release.env")
	candidate.ActiveSlot = slot
	if slot != "" {
		candidate.Layout = LayoutBlueGreen
	}
	if err := safeDirectory(directory, 0700); err != nil {
		return candidate, err
	}
	template := release.ComposeTemplate
	if slot != "" {
		var err error
		template, _, err = renderTopology(release.ComposeTemplate, candidate, slot, service.instanceDirectory(config.ID))
		if err != nil {
			return candidate, err
		}
	}
	env, err := os.ReadFile(config.EnvironmentFile)
	if err != nil {
		return candidate, err
	}
	pg, redis, err := release.InfrastructureImages(config.PostgresMajor, config.RedisMajor)
	if err != nil {
		return candidate, err
	}
	env, err = replaceEnvironmentValues(env, map[string]string{
		"GEOFLOW_RELEASE_SEQUENCE": strconv.FormatUint(release.Sequence, 10), "GEOFLOW_VERSION": release.Version,
		"GEOFLOW_APP_IMAGE": release.AppImage, "GEOFLOW_WEB_IMAGE": release.WebImage,
		"GEOFLOW_POSTGRES_IMAGE": pg, "GEOFLOW_REDIS_IMAGE": redis,
	})
	if err != nil {
		return candidate, err
	}
	for path, data := range map[string][]byte{candidate.ComposeFile: template, candidate.EnvironmentFile: env, filepath.Join(directory, "upgrade-plan.json"): release.UpgradePlan, filepath.Join(directory, "version.json"): release.VersionDocument} {
		mode := os.FileMode(0600)
		if filepath.Base(path) == "upgrade-plan.json" {
			// Signed public metadata is mounted read-only into the unprivileged CLI.
			mode = 0644
		}
		if err := replaceContents(path, data, mode); err != nil {
			return candidate, err
		}
	}
	if slot != "" {
		if err := service.prepareViews(candidate); err != nil {
			return candidate, err
		}
	}
	if err := service.command(ctx, candidate, "config", "--quiet"); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func safeDirectory(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) {
		return errors.New("managed directory must be absolute")
	}
	// Check each component before creating descendants so a symlink cannot redirect writes.
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil {
				return err
			}
			// The daemon umask must not remove access required by container users.
			if err := os.Chmod(current, mode); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed directory contains a symbolic link or non-directory")
		}
	}

	return nil
}

func (service *Service) applicationPhase(ctx context.Context, config instance.Config, release managed.Release, source uint64, operation, phase string, drained bool) (applicationReport, error) {
	var report applicationReport
	planPath := filepath.Join(filepath.Dir(config.ComposeFile), "upgrade-plan.json")
	args := []string{"run", "--rm", "--no-deps", "--user", "33:33", "--label", "geoflow.updater.operation=" + operation, "--entrypoint", "php", "-v", planPath + ":/run/geoflow-upgrade-plan.json:ro", "-e", "AUTO_MIGRATE=false", "-e", "AUTO_INSTALL_ONCE=false", "-e", "AUTO_OPTIMIZE=false", "-e", "GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=false"}
	confirmation := "false"
	if drained {
		confirmation = "true"
	}
	args = append(args, "-e", "GEOFLOW_SECURITY_UPGRADE_DRAIN_CONFIRMED="+confirmation, "init", "artisan", "geoflow:upgrade", "--phase="+phase, "--strategy="+executionStrategy(release, drained), "--operation="+operation, "--source-sequence="+strconv.FormatUint(source, 10), "--plan=/run/geoflow-upgrade-plan.json", "--plan-sha256="+managed.PlanSHA256(release.UpgradePlan), "--json", "--no-interaction")
	var out boundedOutput
	err := service.runner().Run(ctx, nil, &out, "docker", append(composeArguments(config.Root, config.EnvironmentFile, config.ComposeFile), args...)...)
	if err != nil {
		return report, fmt.Errorf("application upgrade %s failed: %w", phase, err)
	}
	if out.exceeded {
		return report, errors.New("application upgrade output exceeded limit")
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		return report, fmt.Errorf("application upgrade %s returned invalid JSON", phase)
	}
	if report.SchemaVersion != 1 || report.Status != "pass" || report.PlanSHA256 != managed.PlanSHA256(release.UpgradePlan) || report.Version != release.Version {
		return report, errors.New("application readiness or release identity did not match the signed plan")
	}
	return report, nil
}

type boundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (out *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if out.Len()+n > 2*1024*1024 {
		out.exceeded = true
		return n, nil
	}
	return out.Buffer.Write(data)
}

func siteAuthority(root string) (string, string, int, error) {
	env, err := os.ReadFile(filepath.Join(root, ".env.prod"))
	if err != nil {
		return "", "", 0, err
	}
	address, err := environmentValue(env, "APP_URL")
	if err != nil {
		return "", "", 0, err
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || strings.ContainsAny(parsed.Host, "\r\n") {
		return "", "", 0, errors.New("APP_URL must be an absolute HTTP or HTTPS URL without credentials")
	}
	port := 80
	if parsed.Scheme == "https" {
		port = 443
	}
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", "", 0, errors.New("APP_URL port is invalid")
		}
	}
	return parsed.Host, parsed.Scheme, port, nil
}
func (service *Service) checkHTTP(ctx context.Context, config instance.Config) error {
	host, _, _, err := siteAuthority(config.Root)
	if err != nil {
		return err
	}
	for _, path := range []string{"/up", "/"} {
		if err := service.command(ctx, config, "exec", "-T", "web", "wget", "-q", "-T", "10", "--header", "Host: "+host, "-O", "/dev/null", "http://127.0.0.1"+path); err != nil {
			return fmt.Errorf("candidate HTTP check %s failed: %w", path, err)
		}
	}
	return nil
}

func (service *Service) containerIDs(ctx context.Context, config instance.Config, names ...string) ([]string, error) {
	args := append([]string{"ps", "--all", "--quiet"}, names...)
	out, err := service.output(ctx, config, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	for _, id := range ids {
		if len(id) < 12 || len(id) > 64 || strings.Trim(id, "0123456789abcdef") != "" {
			return nil, errors.New("invalid Docker container identifier")
		}
	}
	return ids, nil
}

func (service *Service) drainServices(ctx context.Context, config instance.Config, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if len(names) > 1 {
		for _, name := range names {
			if name == "web" || name == "app" {
				for _, single := range names {
					if err := service.drainServices(ctx, config, single); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
	ids, err := service.containerIDs(ctx, config, names...)
	if err != nil {
		return err
	}
	signal := "TERM"
	if len(names) == 1 && (names[0] == "web" || names[0] == "app") {
		signal = "QUIT"
	}
	return service.drainContainersSignal(ctx, ids, signal)
}
func (service *Service) drainContainers(ctx context.Context, ids []string) error {
	return service.drainContainersSignal(ctx, ids, "TERM")
}
func (service *Service) drainContainersSignal(ctx context.Context, ids []string, signal string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := service.runner().Run(ctx, nil, io.Discard, "docker", append([]string{"update", "--restart=no"}, ids...)...); err != nil {
		return err
	}
	for _, id := range ids {
		var out limitedBuffer
		if err := service.runner().Run(ctx, nil, &out, "docker", "inspect", "--format={{.State.Running}}", id); err != nil {
			return err
		}
		if strings.TrimSpace(out.String()) == "true" {
			if err := service.runner().Run(ctx, nil, io.Discard, "docker", "kill", "--signal="+signal, id); err != nil {
				return err
			}
		}
	}
	for {
		running := false
		for _, id := range ids {
			var out limitedBuffer
			if err := service.runner().Run(ctx, nil, &out, "docker", "inspect", "--format={{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}", id); err != nil {
				return err
			}
			parts := strings.Split(strings.TrimSpace(out.String()), "|")
			if len(parts) != 3 || parts[2] != "false" {
				return errors.New("worker state or OOM check failed")
			}
			if parts[0] == "true" {
				running = true
				continue
			}
			if parts[0] != "false" || (parts[1] != "0" && parts[1] != "143" && !(signal == "KILL" && parts[1] == "137")) {
				return fmt.Errorf("worker did not drain cleanly (exit %s)", parts[1])
			}
		}
		if !running {
			return nil
		}
		if err := waitContext(ctx, time.Second); err != nil {
			return fmt.Errorf("drain deadline exceeded; containers retained: %w", err)
		}
	}
}
func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func applicationServices(composeFile string) ([]string, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, err
	}
	var document struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	var names []string
	for name := range document.Services {
		if !composeServicePattern.MatchString(name) {
			return nil, errors.New("invalid service name")
		}
		if name != "postgres" && name != "redis" && name != "init" && name != "system-update-queue" && name != "edge" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (service *Service) startServices(ctx context.Context, config instance.Config, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	ids, err := service.containerIDs(ctx, config, names...)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := service.runner().Run(ctx, nil, io.Discard, "docker", append([]string{"update", "--restart=unless-stopped"}, ids...)...); err != nil {
			return err
		}
	}
	// Readiness checks must see healthy containers after startup. HTTP can respond
	// before Docker completes its first health check, including during recovery.
	return service.command(ctx, config, append([]string{"up", "-d", "--no-deps", "--wait", "--wait-timeout", "180"}, names...)...)
}

func (service *Service) copyAssets(ctx context.Context, config instance.Config) error {
	ids, err := service.containerIDs(ctx, config, "web")
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return errors.New("candidate web container is missing or scaled unexpectedly")
	}
	directory := filepath.Join(service.instanceDirectory(config.ID), "assets")
	if err := safeDirectory(directory, 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(service.instanceDirectory(config.ID), ".assets-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if err := service.runner().Run(ctx, nil, &sizeLimitedWriter{Writer: temporary, Remaining: 256 * 1024 * 1024}, "docker", "cp", ids[0]+":/var/www/html/public/build/assets/.", "-"); err != nil {
		return fmt.Errorf("export signed frontend assets: %w", err)
	}
	if _, err := temporary.Seek(0, 0); err != nil {
		return err
	}
	reader := tar.NewReader(io.LimitReader(temporary, 256*1024*1024))
	count := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		name = strings.TrimPrefix(name, "assets/")
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg || name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "..") || strings.Contains(name, "\\") || header.Size < 0 || header.Size > 32*1024*1024 {
			return errors.New("frontend asset archive has an unsafe entry")
		}
		contents, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(contents)) != header.Size {
			return errors.New("frontend asset archive is truncated")
		}
		destination := filepath.Join(directory, name)
		if err := safeDirectory(filepath.Dir(destination), 0755); err != nil {
			return err
		}
		old, err := os.ReadFile(destination)
		if err == nil {
			if !bytes.Equal(old, contents) {
				return errors.New("frontend asset name collides with retained release content")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := replaceContents(destination, contents, 0644); err != nil {
				return err
			}
		} else {
			return err
		}
		count++
	}
	if count == 0 {
		return errors.New("candidate contains no frontend build assets")
	}
	return nil
}

func executionStrategy(release managed.Release, drained bool) string {
	if drained {
		return managed.StrategyMaintenance
	}
	return releaseStrategy(release)
}

func (service *Service) waitUpgradeContainers(ctx context.Context, operation string) error {
	for {
		var out limitedBuffer
		if err := service.runner().Run(ctx, nil, &out, "docker", "ps", "--quiet", "--filter", "label=geoflow.updater.operation="+operation); err != nil {
			return err
		}
		if strings.TrimSpace(out.String()) == "" {
			return nil
		}
		if err := waitContext(ctx, time.Second); err != nil {
			return fmt.Errorf("upgrade process remains active; recovery deferred: %w", err)
		}
	}
}

func (service *Service) drainRetiredWorker(ctx context.Context) error {
	var out limitedBuffer
	if err := service.runner().Run(ctx, nil, &out, "docker", "ps", "--all", "--quiet", "--filter", "name=^/geoflow-system-update-queue-prod$"); err != nil {
		return err
	}
	ids := strings.Fields(out.String())
	for _, id := range ids {
		if len(id) < 12 || strings.Trim(id, "0123456789abcdef") != "" {
			return errors.New("invalid retired worker identifier")
		}
	}
	if err := service.drainContainers(ctx, ids); err != nil {
		return err
	}
	if len(ids) > 0 {
		return service.runner().Run(ctx, nil, io.Discard, "docker", append([]string{"rm"}, ids...)...)
	}
	return nil
}

type sizeLimitedWriter struct {
	Writer    io.Writer
	Remaining int64
}

func (w *sizeLimitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.Remaining {
		return 0, errors.New("artifact exceeds backup byte limit")
	}
	n, err := w.Writer.Write(data)
	w.Remaining -= int64(n)
	return n, err
}
