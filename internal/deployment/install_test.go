package deployment

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/managed"
)

type releaseResolverFunc func(context.Context) (managed.Release, error)

func (f releaseResolverFunc) Current(c context.Context) (managed.Release, error) { return f(c) }

type provisionerFunc func(enrollment.Request, managed.Release) (enrollment.Result, error)

func (f provisionerFunc) ProvisionFresh(r enrollment.Request, v managed.Release) (enrollment.Result, error) {
	return f(r, v)
}
func TestFreshInstallAndRetryKeepCredentialsAndData(t *testing.T) {
	for _, fault := range []string{"none", "site-files", "enrolled", "torn-token", "seed-admin"} {
		t.Run(fault, func(t *testing.T) { testInstallRetry(t, fault) })
	}
}
func testInstallRetry(t *testing.T, fault string) {
	state := canonicalTemp(t)
	root := filepath.Join(installSiteParent(t), "site")
	release := testRelease(t, false)
	var calls []string
	adminSeeded, knowledgeSynced := false, false
	runner := functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		switch {
		case strings.Contains(cmd, "artisan geoflow:install"):
			if fault == "seed-admin" && !adminSeeded {
				adminSeeded = true
				return errors.New("injected interruption after admin seeding")
			}
		case strings.Contains(cmd, "artisan geoflow:sync-system-knowledge --media"):
			knowledgeSynced = true
		case strings.Contains(cmd, "artisan geoflow:backfill-ai-quality-retrieval"):
			if !knowledgeSynced {
				return errors.New("system knowledge must be repaired before retrieval backfill")
			}
		case strings.Contains(cmd, "geoflow:upgrade"):
			if !knowledgeSynced {
				return errors.New("system knowledge readiness failed")
			}
			return json.NewEncoder(out).Encode(applicationReport{SchemaVersion: 1, Status: "pass", Version: release.Version, PlanSHA256: managed.PlanSHA256(release.UpgradePlan)})
		case strings.HasSuffix(cmd, "ps --all --quiet web"):
			_, e := io.WriteString(out, strings.Repeat("a", 12))
			return e
		case strings.Contains(cmd, "Running}}|"):
			_, e := io.WriteString(out, "false|0|false")
			return e
		case strings.Contains(cmd, "Running}}"):
			_, e := io.WriteString(out, "false")
			return e
		case len(args) > 0 && args[0] == "cp":
			writer := tar.NewWriter(out)
			data := []byte("fresh asset")
			if e := writer.WriteHeader(&tar.Header{Name: "app-abcd.js", Size: int64(len(data)), Mode: 0644}); e != nil {
				return e
			}
			if _, e := writer.Write(data); e != nil {
				return e
			}
			return writer.Close()
		case strings.Contains(cmd, "8081/release"):
			return json.NewEncoder(out).Encode(map[string]any{"slot": "blue", "sequence": release.Sequence})
		case strings.Contains(cmd, "ps --all --format json"):
			var compose string
			for i, arg := range args {
				if arg == "-f" {
					compose = args[i+1]
				}
			}
			names, e := applicationServices(compose)
			if e != nil {
				return e
			}
			for _, name := range names {
				if e := json.NewEncoder(out).Encode(map[string]string{"Service": name, "State": "running", "Health": "healthy"}); e != nil {
					return e
				}
			}
		}
		return nil
	})
	noChown := func(string, int, int) error { return nil }
	service := Service{StateDir: state, Runner: runner, Releases: releaseResolverFunc(func(context.Context) (managed.Release, error) { return release, nil }), Provisioner: enrollment.Service{StateDir: state, Chown: noChown}, Chown: noChown}
	request := InstallRequest{InstanceID: "primary", Root: root, URL: "https://example.test"}
	if fault != "none" {
		injected := false
		if fault == "seed-admin" {
			// The runner simulates geoflow:install skipping seeders after a partial first run.
		} else if fault == "site-files" {
			service.Chown = func(string, int, int) error {
				if !injected {
					injected = true
					return errors.New("injected site file interruption")
				}
				return nil
			}
		} else {
			original := service.Provisioner
			service.Provisioner = provisionerFunc(func(request enrollment.Request, release managed.Release) (enrollment.Result, error) {
				result, err := original.ProvisionFresh(request, release)
				if err == nil && !injected {
					injected = true
					if fault == "torn-token" {
						writeTest(t, result.Instance.ControlToken, []byte(""))
						writeTest(t, filepath.Join(filepath.Dir(result.Instance.ControlToken), ".geoflow-updater-12345"), []byte("incomplete journal temporary"))
					}
					return result, errors.New("injected enrollment interruption")
				}
				return result, err
			})
		}
		if _, err := service.Install(context.Background(), request); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("fault not reached: %v", err)
		}
		before, err := os.ReadFile(filepath.Join(root, ".env.prod"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			after, err := os.ReadFile(filepath.Join(root, ".env.prod"))
			if err != nil || string(before) != string(after) {
				t.Error("interrupted preparation rotated secrets")
			}
		}()
	}
	result, e := service.Install(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	if result.Instance.ActiveSlot != "blue" || result.Instance.ReleaseSequence != release.Sequence {
		t.Fatalf("wrong installation identity: %+v", result.Instance)
	}
	password, e := os.ReadFile(result.CredentialsFile)
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(result.CredentialsFile)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credentials are not private")
	}
	env, e := os.ReadFile(filepath.Join(root, ".env.prod"))
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "down --remove-orphans") || strings.Contains(joined, "docker system") || strings.Contains(joined, string(password)) {
		t.Fatal("installation touched unrelated resources or logged credentials")
	}
	for _, command := range []string{"artisan migrate --force", "artisan geoflow:install", "--phase=verify"} {
		if !strings.Contains(joined, command) {
			t.Fatal("missing initialization step", command)
		}
	}
	if strings.Contains(joined, "--phase=apply") {
		t.Fatal("fresh installation spoofed an upgrade source sequence")
	}
	callCount := len(calls)
	again, e := service.Install(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	if again.CredentialsFile != result.CredentialsFile || len(calls) != callCount {
		t.Fatal("completed install reran initialization")
	}
	second, e := os.ReadFile(filepath.Join(root, ".env.prod"))
	if e != nil || string(second) != string(env) {
		t.Fatal("retry rotated application identity")
	}
}

func installSiteParent(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		return canonicalTemp(t)
	}
	// Linux's default /tmp is intentionally refused by enrollment. A separate
	// tmpfs fixture exercises the unchanged production guard and real install path.
	parent, err := os.MkdirTemp("/dev/shm", "geoflow-install-test-")
	if err != nil {
		t.Fatalf("create isolated installation fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(parent); err != nil {
			t.Errorf("remove isolated installation fixture: %v", err)
		}
	})
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := enrollment.ValidateRootAccess(resolved); err != nil {
		t.Fatalf("installation fixture must pass the production root guard: %v", err)
	}
	return resolved
}

func TestFreshInstallPreservesServiceSandboxGuard(t *testing.T) {
	service := Service{StateDir: canonicalTemp(t)}
	_, err := service.Install(context.Background(), InstallRequest{
		InstanceID: "primary", Root: "/tmp/geoflow-install-forbidden", URL: "https://example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked by the installed systemd sandbox") {
		t.Fatalf("installation must reject hidden sandbox roots before resolving a release: %v", err)
	}
}

func TestNewSiteEnvironmentUsesIndependentRandomSecrets(t *testing.T) {
	origin, _ := url.Parse("https://site.test:8443")
	first, password, e := newSiteEnvironment(origin)
	if e != nil {
		t.Fatal(e)
	}
	second, password2, e := newSiteEnvironment(origin)
	if e != nil {
		t.Fatal(e)
	}
	if password == password2 || string(first) == string(second) {
		t.Fatal("installation credentials repeated")
	}
	key, e := environmentValue(first, "APP_KEY")
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(key, "base64:"))
	if e != nil || len(decoded) != 32 {
		t.Fatal("invalid application key")
	}
	seen := map[string]bool{}
	for _, name := range []string{"GEOFLOW_ADMIN_PASSWORD", "DB_PASSWORD", "REDIS_PASSWORD", "REVERB_APP_KEY", "REVERB_APP_SECRET"} {
		value, e := environmentValue(first, name)
		if e != nil || len(value) < 32 || seen[value] {
			t.Fatalf("weak or reused %s", name)
		}
		seen[value] = true
	}
	if !strings.Contains(string(first), "GEOFLOW_NGINX_PUBLIC_PORT=8443") {
		t.Fatal("public port not preserved")
	}
}

func TestFreshInstallRefusesExistingContentAndUnsafeURL(t *testing.T) {
	for _, address := range []string{"https://user:password@site.test", "https://site.test/path", "https://site.test?x=y", "https://site.test\nATTACK=x"} {
		service := Service{StateDir: canonicalTemp(t)}
		if _, e := service.Install(context.Background(), InstallRequest{InstanceID: "primary", Root: filepath.Join(canonicalTemp(t), "site"), URL: address}); e == nil {
			t.Fatalf("accepted unsafe URL %q", address)
		}
	}
	root := canonicalTemp(t)
	writeTest(t, filepath.Join(root, "keep.txt"), []byte("existing content"))
	if e := prepareEmptyRoot(root); e == nil {
		t.Fatal("accepted existing site")
	}
	data, e := os.ReadFile(filepath.Join(root, "keep.txt"))
	if e != nil || string(data) != "existing content" {
		t.Fatal("modified existing site")
	}
}
