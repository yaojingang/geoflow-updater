package deployment

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"gopkg.in/yaml.v3"
)

func runtimeTopology(t *testing.T) map[string]struct {
	User        string            `yaml:"user"`
	Environment map[string]string `yaml:"environment"`
} {
	t.Helper()
	template, err := os.ReadFile("../../assets/docker-compose.managed.yml")
	if err != nil {
		t.Fatal(err)
	}
	app, _, err := renderTopology(template, instance.Config{ID: "primary", ReleaseSequence: 9}, "blue", "/state/instances/primary")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			User        string            `yaml:"user"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(app, &document); err != nil {
		t.Fatal(err)
	}
	return document.Services
}

func TestTopologyRuntimeProcessesUseSharedStorageOwner(t *testing.T) {
	for name, role := range runtimeTopology(t) {
		if name == "web" {
			continue
		}
		if name != "app" && role.User != "33:33" {
			t.Errorf("%s writes private storage as %q; PHP-FPM uses UID 33", name, role.User)
		}
		if role.Environment["AUTO_OPTIMIZE"] != "false" {
			t.Errorf("%s can erase the warmed slot views during startup", name)
		}
	}
}

func TestInstallCommandUsesRuntimeUser(t *testing.T) {
	runner := &recordingRunner{}
	service := Service{Runner: runner}
	if err := service.installCommand(context.Background(), instance.Config{}, "install-test", false, "geoflow:install"); err != nil {
		t.Fatal(err)
	}
	args := runner.commands[0].arguments
	index := slices.Index(args, "--user")
	if index < 0 || args[index+1] != "33:33" {
		t.Fatalf("installation writes storage as the image default user: %v", args)
	}
}

func TestSafeDirectoryHonorsPermissionsUnderDaemonUmask(t *testing.T) {
	const child = "GEOFLOW_DEPLOY_TEST_UMASK_CHILD"
	const destination = "GEOFLOW_DEPLOY_TEST_UMASK_PATH"
	if os.Getenv(child) == "1" {
		syscall.Umask(0027)
		path := os.Getenv(destination)
		if err := safeDirectory(path, 0755); err != nil {
			t.Fatal(err)
		}
		for _, directory := range []string{path, filepath.Dir(path)} {
			info, err := os.Stat(directory)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0755 {
				t.Fatalf("Nginx asset directory mode = %o, want 755 under daemon UMask=0027", info.Mode().Perm())
			}
		}
		parent, err := os.Stat(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			t.Fatal(err)
		}
		if parent.Mode().Perm() != 0700 {
			t.Fatalf("existing private parent mode changed to %o, want 700", parent.Mode().Perm())
		}
		return
	}
	parent := canonicalTemp(t)
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "assets", "nested")
	command := exec.Command(os.Args[0], "-test.run=^TestSafeDirectoryHonorsPermissionsUnderDaemonUmask$")
	command.Env = append(os.Environ(), child+"=1", destination+"="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("daemon-umask subprocess: %v\n%s", err, output)
	}
}

func dockerRuntime(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestDockerIngressRuntimeStorageOwner(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise Linux storage ownership")
	}
	volume := fmt.Sprintf("geoflow-storage-test-%d", time.Now().UnixNano())
	dockerRuntime(t, "volume", "create", volume)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", volume).Run() })
	image := "python:3.12-alpine"
	dockerRuntime(t, "run", "--rm", "--network", "none", "-v", volume+":/proof", image, "python", "-c", "import os; os.mkdir('/proof/private'); os.chown('/proof/private',33,33)")
	for _, role := range []string{"init", "queue"} {
		user := runtimeTopology(t)[role].User
		args := []string{"run", "--rm", "--network", "none", "-v", volume + ":/proof"}
		if user != "" {
			args = append(args, "--user", user)
		}
		program := fmt.Sprintf("import os; os.mkdir('/proof/private/%s',0o700); open('/proof/private/%s/asset','w').write('shared')", role, role)
		dockerRuntime(t, append(args, image, "python", "-c", program)...)
		read := fmt.Sprintf("assert open('/proof/private/%s/asset').read() == 'shared'", role)
		dockerRuntime(t, "run", "--rm", "--network", "none", "--user", "33:33", "-v", volume+":/proof", image, "python", "-c", read)
	}
}

func TestDockerIngressLegacyResumeRestoresRestartPolicy(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise Compose restart policies")
	}
	state := canonicalTemp(t)
	root := filepath.Join(state, "site")
	dir := filepath.Join(state, "instances", "primary")
	config := instance.Config{SchemaVersion: 1, ID: "primary", Root: root, ComposeFile: filepath.Join(dir, "compose.yml"), EnvironmentFile: filepath.Join(dir, "release.env"), ControlToken: filepath.Join(dir, "control.token"), ReleaseSequence: 7, Version: "3.0.0"}
	project := fmt.Sprintf("geoflow-restart-test-%d", time.Now().UnixNano())
	compose := "name: " + project + "\nservices:\n"
	for _, name := range []string{"postgres", "redis", "init", "app", "web", "queue", "knowledge-queue", "scheduler", "reverb"} {
		compose += "  " + name + ":\n    image: nginx:1.31.1-alpine\n    network_mode: none\n    restart: unless-stopped\n"
	}
	for path, data := range map[string]string{config.ComposeFile: compose, config.EnvironmentFile: "", config.ControlToken: "token", filepath.Join(root, ".env.prod"): "", filepath.Join(root, "version.json"): "{}"} {
		writeTest(t, path, []byte(data))
	}
	if err := os.MkdirAll(filepath.Join(root, "storage"), 0755); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(dir, "instance.yml"), encoded)
	arguments := composeArguments(root, config.EnvironmentFile, config.ComposeFile)
	t.Cleanup(func() { _ = exec.Command("docker", append(arguments, "down", "--remove-orphans")...).Run() })
	dockerRuntime(t, append(arguments, "up", "-d", "app")...)
	id := dockerRuntime(t, append(arguments, "ps", "--quiet", "app")...)
	dockerRuntime(t, "update", "--restart=no", id)
	dockerRuntime(t, "kill", "--signal=QUIT", id)
	dockerRuntime(t, "wait", id)
	service := Service{StateDir: state, Runner: functionRunner(func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		if slices.Contains(args, "run") || len(args) > 0 && args[0] == "rm" {
			return nil
		}
		if slices.Contains(args, "up") {
			args = append(composeArguments(root, config.EnvironmentFile, config.ComposeFile), "up", "-d", "--wait", "app")
		}
		return (RealRunner{}).Run(ctx, in, out, name, args...)
	})}
	if err := service.Resume(context.Background(), "primary"); err != nil {
		t.Fatal(err)
	}
	if got := dockerRuntime(t, "inspect", "--format={{.HostConfig.RestartPolicy.Name}}", id); got != "unless-stopped" {
		t.Fatalf("resumed service restart policy = %s; max-jobs/max-time workers will stay stopped", got)
	}
}
