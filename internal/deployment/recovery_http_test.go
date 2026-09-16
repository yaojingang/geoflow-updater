package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"gopkg.in/yaml.v3"
)

const recoveryWebFixture = "ghcr.io/yaojingang/geoflow-web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type recoveryHTTPDocker struct {
	t                         *testing.T
	service                   *Service
	config                    instance.Config
	failure                   string
	created, started, enabled bool
	oldIDs, newIDs            map[string]string
	verified                  map[string]bool
	before                    functionRunner
}

func newRecoveryHTTPDocker(t *testing.T, service *Service, config instance.Config) *recoveryHTTPDocker {
	t.Helper()
	return &recoveryHTTPDocker{t: t, service: service, config: config,
		oldIDs: map[string]string{"app": strings.Repeat("a", 64), "web": strings.Repeat("b", 64)},
		newIDs: map[string]string{"app": strings.Repeat("c", 64), "web": strings.Repeat("d", 64)}, verified: map[string]bool{}}
}

func (d *recoveryHTTPDocker) Run(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	if d.before != nil {
		if err := d.before.Run(ctx, in, out, name, args...); err != nil {
			return err
		}
	}
	cmd := strings.Join(args, " ")
	crash := errors.New("simulated process crash")
	if strings.Contains(cmd, "ps --all --quiet") {
		ids := d.oldIDs
		if d.created {
			ids = d.newIDs
		}
		for _, role := range []string{"app", "web"} {
			if slices.Contains(args, role) {
				_, _ = io.WriteString(out, ids[role]+"\n")
			}
		}
		return nil
	}
	if len(args) > 1 && args[0] == "update" {
		if args[1] == "--restart=unless-stopped" {
			for _, id := range args[2:] {
				for _, old := range d.oldIDs {
					if id == old {
						d.t.Fatal("naturally exited old container can restart without the recovery guard")
					}
				}
				if !d.verified[id] {
					d.t.Fatal("restart enabled before new container inspection")
				}
			}
			if d.failure == "before-enable" {
				return crash
			}
			d.enabled = true
			if d.failure == "after-enable" {
				return crash
			}
		}
		return nil
	}
	if slices.Contains(args, "up") && slices.Contains(args, "--no-start") {
		if !slices.Contains(args, "--force-recreate") || !slices.Contains(args, "--no-deps") {
			d.t.Fatal("recovery reused old containers or started dependencies")
		}
		last := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-f" {
				last = args[i+1]
			}
		}
		data, err := os.ReadFile(last)
		if err != nil {
			d.t.Fatal(err)
		}
		var overlay struct {
			Services map[string]struct {
				Restart string `yaml:"restart"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(data, &overlay) != nil || overlay.Services["app"].Restart != "no" || overlay.Services["web"].Restart != "no" {
			d.t.Fatal("new container configuration does not persist restart=no")
		}
		if d.failure == "before-create" {
			return crash
		}
		d.created = true
		if d.failure == "after-create" {
			return crash
		}
		return nil
	}
	if len(args) > 0 && args[0] == "inspect" {
		id := args[len(args)-1]
		for _, old := range d.oldIDs {
			if id == old {
				if strings.Contains(cmd, "ExitCode") {
					_, _ = io.WriteString(out, "false|0|false")
				} else {
					_, _ = io.WriteString(out, "false")
				}
				return nil
			}
		}
		role := ""
		for name, value := range d.newIDs {
			if id == value {
				role = name
			}
		}
		if role == "" {
			return errors.New("unknown container")
		}
		if d.failure == "inspect-error" {
			return crash
		}
		image := recoveryWebFixture
		env := []string{}
		mounts := []map[string]any{}
		if role == "app" {
			data, err := os.ReadFile(d.config.EnvironmentFile)
			if err != nil {
				d.t.Fatal(err)
			}
			image, err = environmentValue(data, "GEOFLOW_APP_IMAGE")
			if err != nil {
				d.t.Fatal(err)
			}
			env = []string{"GEOFLOW_RECOVERY_CONTRACT=1", "GEOFLOW_UPDATER_INSTANCE_ID=primary", "AUTO_MIGRATE=false", "AUTO_INSTALL_ONCE=false", "AUTO_OPTIMIZE=false"}
			add := func(source, dest string) {
				mounts = append(mounts, map[string]any{"Type": "bind", "Source": source, "Destination": dest, "RW": false})
			}
			if d.failure != "missing-state" {
				add(d.service.recoveryControl().PublicDir("primary"), "/run/geoflow-recovery-control")
			}
			if image == legacyRecoveryAppImage {
				dir := filepath.Join(d.service.StateDir, "recovery-control", "primary", "legacy-adapter")
				add(dir, legacyAdapterContainerDir)
				if d.failure != "missing-guard" {
					add(filepath.Join(dir, "legacy-recovery.ini"), "/usr/local/etc/php/conf.d/zz-geoflow-recovery.ini")
				}
			}
			if d.failure == "writable-state" {
				mounts[0]["RW"] = true
			}
			if d.failure == "bad-env" {
				env = nil
			}
			if d.failure == "ini-bypass" {
				env = append(env, "PHP_INI_SCAN_DIR=/tmp")
			}
			if d.failure == "wrong-image" {
				image = "other@sha256:" + strings.Repeat("f", 64)
			}
		}
		policy := "no"
		if d.enabled {
			policy = "unless-stopped"
		}
		d.verified[id] = true
		return json.NewEncoder(out).Encode(map[string]any{"Id": id, "Config": map[string]any{"Image": image, "Env": env, "Labels": map[string]string{"com.docker.compose.service": role}}, "HostConfig": map[string]any{"RestartPolicy": map[string]string{"Name": policy}}, "State": map[string]any{"Running": d.started, "Status": map[bool]string{false: "created", true: "running"}[d.started], "Health": map[string]string{"Status": "healthy"}}, "Mounts": mounts})
	}
	if len(args) > 0 && args[0] == "start" {
		if !d.created || !d.enabled {
			d.t.Fatal("HTTP started before guarded configuration was committed and verified")
		}
		for _, id := range args[1:] {
			if !d.verified[id] {
				d.t.Fatal("HTTP started an unverified container")
			}
		}
		if d.failure == "before-start" {
			return crash
		}
		d.started = true
		if d.failure == "after-start" {
			return crash
		}
	}
	return nil
}

func TestRecoveredHTTPStartsOnlyVerifiedNewContainers(t *testing.T) {
	for _, failure := range []string{"before-create", "after-create", "inspect-error", "missing-state", "missing-guard", "writable-state", "bad-env", "ini-bypass", "wrong-image", "before-enable", "after-enable", "before-start", "after-start", ""} {
		t.Run(failure, func(t *testing.T) {
			service, config, _, _ := recoveryTopologyFixture(t)
			config.Version = "3.1.0"
			writeTest(t, config.EnvironmentFile, []byte("GEOFLOW_VERSION=3.1.0\nGEOFLOW_APP_IMAGE="+legacyRecoveryAppImage+"\nGEOFLOW_WEB_IMAGE="+recoveryWebFixture+"\n"))
			encoded, _ := yaml.Marshal(config)
			writeTest(t, filepath.Join(service.instanceDirectory(config.ID), "instance.yml"), encoded)
			control := service.recoveryControl()
			_, err := control.Begin(config.ID, "20260827T123456Z-1234abcd", "recovery-http-transaction", strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = control.Restored(config.ID, "recovery-http-transaction"); err != nil {
				t.Fatal(err)
			}
			if _, err = control.RecordPreparation(config.ID, "recovery-http-transaction", strings.Repeat("b", 64)); err != nil {
				t.Fatal(err)
			}
			if _, err = control.RecordRedisManifest(config.ID, "recovery-http-transaction", strings.Repeat("c", 64)); err != nil {
				t.Fatal(err)
			}
			if _, err = control.OpenHTTP(config.ID, "recovery-http-transaction"); err != nil {
				t.Fatal(err)
			}
			docker := newRecoveryHTTPDocker(t, service, config)
			docker.failure = failure
			service.Runner = docker
			err = service.Resume(context.Background(), config.ID)
			if failure == "" {
				if err != nil || !docker.started {
					t.Fatalf("verified HTTP did not start: %v", err)
				}
			} else if err == nil {
				t.Fatal("recovery ignored crash or invalid container configuration")
			}
			if slices.Contains([]string{"before-create", "after-create", "inspect-error", "missing-state", "missing-guard", "writable-state", "bad-env", "ini-bypass", "wrong-image", "before-enable"}, failure) && docker.enabled {
				t.Fatal("failed validation enabled restart")
			}
		})
	}
}
