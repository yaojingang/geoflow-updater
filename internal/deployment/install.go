package deployment

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/authorization"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type InstallRequest struct{ InstanceID, Root, URL string }
type InstallResult struct {
	Instance        instance.Config `json:"instance"`
	CredentialsFile string          `json:"credentials_file"`
}
type freshProvisioner interface {
	ProvisionFresh(enrollment.Request, managed.Release) (enrollment.Result, error)
}
type installJournal struct {
	SchemaVersion     int             `json:"schema_version"`
	Status            string          `json:"status"`
	OperationID       string          `json:"operation_id"`
	URL               string          `json:"url"`
	Source            instance.Config `json:"source"`
	Candidate         instance.Config `json:"candidate"`
	Target            managed.Release `json:"target"`
	EnvironmentSHA256 string          `json:"environment_sha256,omitempty"`
	TrafficOpened     bool            `json:"traffic_opened"`
}

func (service *Service) Install(ctx context.Context, request InstallRequest) (InstallResult, error) {
	empty := InstallResult{}
	request.Root = filepath.Clean(request.Root)
	if request.InstanceID != "primary" || !filepath.IsAbs(request.Root) || filepath.Clean(request.Root) == "/" {
		return empty, errors.New("installation requires primary and an absolute dedicated root")
	}
	authority, err := url.Parse(request.URL)
	if err != nil || authority.Hostname() == "" || (authority.Scheme != "http" && authority.Scheme != "https") || authority.User != nil || authority.RawQuery != "" || authority.Fragment != "" || (authority.Path != "" && authority.Path != "/") || strings.ContainsAny(authority.Host, "\r\n\t \"'${}\\") {
		return empty, errors.New("installation URL must be a plain HTTP(S) origin")
	}
	// Apply the same installed-systemd root restrictions before creating site files.
	if err := enrollment.ValidateRootAccess(request.Root); err != nil {
		return empty, err
	}
	directory := service.instanceDirectory(request.InstanceID)
	if err := safeDirectory(directory, 0750); err != nil {
		return empty, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, "operation.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return empty, errors.New("another instance operation is active")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	journalPath := filepath.Join(directory, "installation.json")
	var journal installJournal
	data, err := os.ReadFile(journalPath)
	if err == nil {
		if len(data) > 8*1024*1024 || json.Unmarshal(data, &journal) != nil || journal.SchemaVersion != 1 || journal.Source.ID != request.InstanceID || journal.Source.Root != filepath.Clean(request.Root) || journal.URL != request.URL || !safeOperationID(journal.OperationID) {
			return empty, errors.New("existing installation does not match requested root and URL")
		}
		if err := journal.Target.Validate(); err != nil {
			return empty, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(filepath.Join(directory, "instance.yml")); !errors.Is(err, os.ErrNotExist) {
			return empty, errors.New("instance is already enrolled; use update")
		}
		release, err := service.Resolve(ctx, request.InstanceID)
		if err != nil {
			return empty, err
		}
		if len(release.UpgradePlan) == 0 {
			return empty, errors.New("fresh managed install requires a signed upgrade-plan release")
		}
		if err := prepareEmptyRoot(request.Root); err != nil {
			return empty, err
		}
		journal = installJournal{SchemaVersion: 1, Status: "preparing", OperationID: fmt.Sprintf("install-%d", time.Now().UnixNano()), URL: request.URL, Source: instance.Config{ID: request.InstanceID, Root: request.Root}, Target: release}
		if err := saveInstall(journalPath, journal); err != nil {
			return empty, err
		}

	} else {
		return empty, err
	}
	if journal.Status == "preparing" {
		if service.Provisioner == nil {
			return empty, errors.New("fresh enrollment is unavailable")
		}
		release := journal.Target
		environment, password, err := preparedSiteEnvironment(request.Root, authority)
		if err != nil {
			return empty, err
		}
		if err := replaceContents(filepath.Join(request.Root, ".env.prod"), environment, 0600); err != nil {
			return empty, err
		}
		if err := service.chown(filepath.Join(request.Root, ".env.prod"), 33, 33); err != nil {
			return empty, err
		}
		for _, relative := range []string{"storage", "storage/app", "storage/app/public", "storage/app/public/uploads/images", "storage/app/private", "storage/app/tmp", "storage/framework", "storage/framework/cache/data", "storage/framework/sessions", "storage/framework/views", "storage/logs"} {
			path := filepath.Join(request.Root, relative)
			if err := safeDirectory(path, 0755); err != nil {
				return empty, err
			}
			if err := service.chown(path, 33, 33); err != nil {
				return empty, err
			}
		}
		for _, relative := range []string{"docker-data/prod/postgres", "docker-data/prod/redis"} {
			if err := safeDirectory(filepath.Join(request.Root, relative), 0755); err != nil {
				return empty, err
			}
		}
		if err := replaceContents(filepath.Join(request.Root, "version.json"), release.VersionDocument, 0640); err != nil {
			return empty, err
		}
		if err := replaceContents(filepath.Join(request.Root, "install-credentials.txt"), []byte("Admin URL: "+request.URL+"/geo_admin\nUsername: admin\nPassword: "+password+"\n"), 0600); err != nil {
			return empty, err
		}
		provisioned, err := service.Provisioner.ProvisionFresh(enrollment.Request{InstanceID: request.InstanceID, Root: request.Root}, release)
		if err != nil {
			return empty, err
		}
		journal.Status = "running"
		journal.Source = provisioned.Instance
		journal.EnvironmentSHA256 = managed.PlanSHA256(environment)
		if err := saveInstall(journalPath, journal); err != nil {
			return empty, err
		}
	}

	result := InstallResult{CredentialsFile: filepath.Join(request.Root, "install-credentials.txt")}

	if journal.Status == "completed" {
		config, err := service.loadConfig(request.InstanceID)
		result.Instance = config
		return result, err
	}
	if journal.EnvironmentSHA256 != "" {
		env, err := os.ReadFile(filepath.Join(request.Root, ".env.prod"))
		if err != nil || managed.PlanSHA256(env) != journal.EnvironmentSHA256 {
			return empty, errors.New("installation environment changed; restore the prepared environment before resuming")
		}
	}
	if err := service.waitUpgradeContainers(ctx, journal.OperationID); err != nil {
		return empty, err
	}
	candidate := journal.Candidate
	if !journal.TrafficOpened {
		candidate, err = service.prepareRelease(ctx, journal.Source, journal.Target, filepath.Join(directory, "slots", "blue"), "blue")
		if err != nil {
			return empty, err
		}
		candidate.InfraComposeFile = filepath.Join(directory, "infra", "docker-compose.yml")
		candidate.InfraEnvironmentFile = filepath.Join(directory, "infra", "release.env")
		journal.Candidate = candidate
		if err := saveInstall(journalPath, journal); err != nil {
			return empty, err
		}
		if err := service.command(ctx, candidate, "pull"); err != nil {
			return empty, err
		}
		if err := service.installInfrastructure(ctx, journal.Source, candidate, journal.Target, true); err != nil {
			return empty, err
		}
		names, err := applicationServices(candidate.ComposeFile)
		if err != nil {
			return empty, err
		}
		if err := service.drainServices(ctx, candidate, names...); err != nil {
			return empty, err
		}
		// The journal owns this initially empty database; all unpublished services are drained.
		if err := service.installCommand(ctx, candidate, journal.OperationID, true, "migrate", "--force"); err != nil {
			return empty, err
		}
		if err := service.installCommand(ctx, candidate, journal.OperationID, false, "geoflow:install"); err != nil {
			return empty, err
		}
		// A partial first-install seed can leave an admin without bundled knowledge.
		// Repair idempotent knowledge/media state before retrieval checks and public traffic.
		for _, command := range [][]string{{"geoflow:sync-system-knowledge", "--media"}, {"geoflow:backfill-ai-quality-retrieval", "--json"}, {"geoflow:managed-images:readiness", "--json"}, {"config:cache"}, {"route:cache"}, {"view:cache"}} {
			stepCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
			err := service.installCommand(stepCtx, candidate, journal.OperationID, false, command...)
			cancel()
			if err != nil {
				return empty, err
			}
		}
		if _, err := service.applicationPhase(ctx, candidate, journal.Target, 0, "", "verify", true); err != nil {
			return empty, err
		}

		if _, err := (authorization.Service{StateDir: service.StateDir}).Provision(request.InstanceID); err != nil {
			return empty, err
		}
		if err := service.startServices(ctx, candidate, "app", "reverb", "web"); err != nil {
			return empty, err
		}
		if err := service.waitHTTP(ctx, candidate); err != nil {
			return empty, err
		}
		if err := service.copyAssets(ctx, candidate); err != nil {
			return empty, err
		}
		journal.TrafficOpened = true
		if err := saveInstall(journalPath, journal); err != nil {
			return empty, err
		}
	}
	// Retrying after opening traffic only completes activation and readiness checks.
	if _, err := service.switchIngress(ctx, candidate, candidate.ReleaseSequence, false); err != nil {
		return empty, err
	}
	if err := service.activateSlot(candidate, journal.Target.VersionDocument); err != nil {
		return empty, err
	}
	names, err := backgroundServices(candidate.ComposeFile)
	if err != nil {
		return empty, err
	}
	if err := service.startServices(ctx, candidate, names...); err != nil {
		return empty, err
	}
	if err := service.runningServices(ctx, candidate); err != nil {
		return empty, err
	}
	if err := service.waitHTTP(ctx, candidate); err != nil {
		return empty, err
	}
	journal.Status = "completed"
	if err := saveInstall(journalPath, journal); err != nil {
		return empty, err
	}
	result.Instance = candidate
	return result, nil
}

func (service *Service) chown(path string, uid, gid int) error {
	if service.Chown != nil {
		return service.Chown(path, uid, gid)
	}
	return os.Chown(path, uid, gid)
}
func saveInstall(path string, journal installJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return replaceContents(path, data, 0600)
}
func prepareEmptyRoot(root string) error {
	if err := safeDirectory(root, 0750); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errors.New("fresh install requires an empty site directory; use enroll for existing sites")
	}
	return nil
}
func randomSecret(size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func newSiteEnvironment(origin *url.URL) ([]byte, string, error) {
	secrets := make([]string, 6)
	for i := range secrets {
		value, err := randomSecret(32)
		if err != nil {
			return nil, "", err
		}
		secrets[i] = value
	}
	key, err := base64.RawURLEncoding.DecodeString(secrets[0])
	if err != nil {
		return nil, "", err
	}
	port := origin.Port()
	if port == "" {
		port = "80"
		if origin.Scheme == "https" {
			port = "443"
		}
	}
	env := fmt.Sprintf(`APP_NAME=GEOFlow
APP_ENV=production
APP_KEY=base64:%s
APP_DEBUG=false
APP_URL=%s
APP_TIMEZONE=Asia/Shanghai
APP_LOCALE=zh_CN
APP_FALLBACK_LOCALE=en
APP_MAINTENANCE_DRIVER=file
TRUSTED_PROXIES=REMOTE_ADDR
GEOFLOW_ADMIN_USERNAME=admin
GEOFLOW_ADMIN_EMAIL=admin@example.invalid
GEOFLOW_ADMIN_PASSWORD=%s
GEOFLOW_INITIAL_ADMIN_HINT_ENABLED=false
GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=false
GEOFLOW_SECURITY_UPGRADE_DRAIN_CONFIRMED=false
GEOFLOW_PRIMARY_HOSTS=%s
GEOFLOW_NGINX_PRIMARY_HOST=%s
GEOFLOW_NGINX_PUBLIC_SCHEME=%s
GEOFLOW_NGINX_PUBLIC_PORT=%s
GEOFLOW_NGINX_HOSTED_ROOT_DOMAIN=invalid
GEOFLOW_HOSTED_SITES_ENABLED=false
WEB_PORT=18080
DB_CONNECTION=pgsql
DB_HOST=postgres
DB_PORT=5432
DB_DATABASE=geoflow
DB_USERNAME=geoflow
DB_PASSWORD=%s
GEOFLOW_UPDATER_POSTGRES_MAJOR=18
GEOFLOW_UPDATER_REDIS_MAJOR=8
REDIS_CLIENT=phpredis
REDIS_HOST=redis
REDIS_PORT=6379
REDIS_PASSWORD=%s
CACHE_STORE=redis
QUEUE_CONNECTION=redis
SESSION_DRIVER=database
SESSION_SECURE_COOKIE=%t
BROADCAST_CONNECTION=reverb
REVERB_APP_ID=geoflow
REVERB_APP_KEY=%s
REVERB_APP_SECRET=%s
REVERB_HOST=%s
REVERB_PORT=%s
REVERB_SCHEME=%s
REVERB_BROADCAST_HOST=reverb
REVERB_BROADCAST_PORT=18080
REVERB_BROADCAST_SCHEME=http
REVERB_SERVER_HOST=0.0.0.0
REVERB_SERVER_PORT=18080
REVERB_SERVER_PATH=/reverb
REVERB_SCALING_ENABLED=true
AUTO_MIGRATE=false
AUTO_INSTALL_ONCE=false
AUTO_FIX_STORAGE_PERMISSIONS=false
AUTO_OPTIMIZE=true
GEOFLOW_UPDATER_INSTANCE_ID=primary
GEOFLOW_UPDATE_CENTER_ENABLED=true
`, base64.StdEncoding.EncodeToString(key), strings.TrimRight(origin.String(), "/"), secrets[1], origin.Hostname(), origin.Hostname(), origin.Scheme, port, secrets[2], secrets[3], origin.Scheme == "https", secrets[4], secrets[5], origin.Hostname(), port, origin.Scheme)
	return []byte(env), secrets[1], nil
}
func (service *Service) installCommand(ctx context.Context, config instance.Config, operation string, fresh bool, command ...string) error {
	flag := "false"
	if fresh {
		flag = "true"
	}
	args := []string{"run", "--rm", "--no-deps", "--label", "geoflow.updater.operation=" + operation, "--user", "33:33", "--entrypoint", "php", "-e", "AUTO_MIGRATE=false", "-e", "AUTO_INSTALL_ONCE=false", "-e", "GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=" + flag, "-e", "GEOFLOW_SECURITY_UPGRADE_DRAIN_CONFIRMED=true", "init", "artisan"}
	args = append(args, command...)
	args = append(args, "--no-interaction")
	return service.command(ctx, config, args...)
}

func preparedSiteEnvironment(root string, origin *url.URL) ([]byte, string, error) {
	path := filepath.Join(root, ".env.prod")
	if err := regularFile(path); errors.Is(err, os.ErrNotExist) {
		return newSiteEnvironment(origin)
	} else if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1024*1024 {
		return nil, "", errors.New("prepared environment is invalid")
	}
	for key, expected := range map[string]string{"APP_URL": strings.TrimRight(origin.String(), "/"), "APP_ENV": "production", "DB_CONNECTION": "pgsql", "DB_HOST": "postgres", "REDIS_HOST": "redis"} {
		value, err := environmentValue(data, key)
		if err != nil || value != expected {
			return nil, "", errors.New("prepared installation environment identity changed")
		}
	}
	key, err := environmentValue(data, "APP_KEY")
	if err != nil {
		return nil, "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(key, "base64:"))
	if err != nil || len(decoded) != 32 {
		return nil, "", errors.New("prepared application key is invalid")
	}
	password, err := environmentValue(data, "GEOFLOW_ADMIN_PASSWORD")
	if err != nil || len(password) < 32 {
		return nil, "", errors.New("prepared administrator password is invalid")
	}
	return data, password, nil
}
